package loadbalancer

import (
	utilipvs "github.com/rmxhaha/kube-proxy-dynamic/pkg/ipvs"
	"github.com/rmxhaha/kube-proxy-dynamic/pkg/load/exchange/collector"
	"k8s.io/client-go/kubernetes"
	"google.golang.org/grpc"
	podloadstore "github.com/rmxhaha/kube-proxy-dynamic/pkg/load/pod/store"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/informers/core/v1"
	corev1 "k8s.io/api/core/v1"
	"time"
	"k8s.io/apimachinery/pkg/util/wait"
	"github.com/docker/libnetwork/ipvs"
	"net"
	"syscall"
	weightstore "github.com/rmxhaha/kube-proxy-dynamic/pkg/weight/pod"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type LoadBalancer struct {
	enforcer *utilipvs.IPVSRuleEnforcer
	collector *collector.Collector
	podloadstore *podloadstore.Store
	endpointInformer cache.SharedIndexInformer
	serviceInformer cache.SharedIndexInformer
	weightprocessor *weightstore.WeightProcessor
}

func NewLoadBalancer(clientset kubernetes.Interface, dialPort int, dialOpts []grpc.DialOption, weightrange uint16, enforcedeleteservice bool) (*LoadBalancer, error) {
	lb := &LoadBalancer{}

	store := podloadstore.New()


	enforcer, err := utilipvs.NewEnforcer(enforcedeleteservice)
	if err != nil {
		return nil, err
	}

	collector, err := collector.New(store, clientset, dialPort, dialOpts)
	if err != nil {
		return nil, err
	}

	serviceInformer := v1.NewServiceInformer(clientset, "", 5 *time.Second, cache.Indexers{})
	endpointInformer := v1.NewEndpointsInformer(clientset, "", 5 *time.Second, cache.Indexers{})

	go serviceInformer.Run(wait.NeverStop)
	go endpointInformer.Run(wait.NeverStop)
	go collector.Run()

	lb.podloadstore = store
	lb.enforcer = enforcer
	lb.collector = collector
	lb.serviceInformer = serviceInformer
	lb.endpointInformer = endpointInformer
	lb.weightprocessor = weightstore.NewWeightProcessor(lb.podloadstore, weightrange)


	return lb, nil
}

type Endpoint struct {
	IP net.IP
	Port uint16
	Weight uint16
}

func (lb *LoadBalancer) Sync(){
	wprocessor := lb.weightprocessor
	vss := utilipvs.VirtualServers{}

	// map[ endpoint namespace, name, port/name ] = [ ip, port ]
	endpointsMap := map[string][] Endpoint{}

	for _, ep := range lb.endpointInformer.GetStore().List() {
		endpoints := ep.(*corev1.Endpoints)

		var ips []string
		for _, ss := range endpoints.Subsets {
			for _, addr := range ss.Addresses {
				ips = append( ips, addr.IP )
			}
		}

		weights := wprocessor.GetWeights(ips)

		for _, ss := range endpoints.Subsets {
			for _, addr := range ss.Addresses {
				for _, port := range ss.Ports {
					var weight uint16 = 1

					if w, ok := weights[addr.IP]; ok{
						weight = uint16(w)
					}

					endpoint := Endpoint{ IP: net.ParseIP(addr.IP), Port: uint16(port.Port), Weight: weight }
					namespacedName := endpoints.Namespace+endpoints.Name
					ipproto := string(port.Protocol)+string(port.Port)
					nameproto := string(port.Protocol)+port.Name

					endpointsMap[namespacedName+ipproto] = append(endpointsMap[namespacedName+ipproto], endpoint)
					endpointsMap[namespacedName+nameproto] = append(endpointsMap[namespacedName+nameproto], endpoint)
				}
			}
		}
	}

	for _, s := range lb.serviceInformer.GetStore().List() {
		srv := s.(*corev1.Service)

		if srv.Spec.ClusterIP == "" || srv.Spec.ClusterIP == "None" {
			continue
		}

		for _, p := range srv.Spec.Ports {
			if p.Port == 0 {
				continue
			}
			ipvssrv := &ipvs.Service{
				Address:       net.ParseIP(srv.Spec.ClusterIP),
				Netmask:       0xffffffff,
				Protocol:      syscall.IPPROTO_TCP,
				Port:          uint16(p.Port),
				SchedName:     "wrr",
				Flags:         0,
				Timeout:       5,
				AddressFamily: syscall.AF_INET,
			}



			if p.Protocol == "TCP" {
				ipvssrv.Protocol = syscall.IPPROTO_TCP
			} else if p.Protocol == "UDP" {
				ipvssrv.Protocol = syscall.IPPROTO_UDP
			}

			vs := utilipvs.NewVirtualServer(ipvssrv)

			namespacedName := srv.Namespace+srv.Name

			var eps []Endpoint
			if p.TargetPort.Type == intstr.Int {
				portid := string(p.Protocol)+string(p.TargetPort.IntVal)
				eps = endpointsMap[namespacedName+portid]
			} else if p.TargetPort.Type == intstr.String {
				portid := string(p.Protocol)+p.TargetPort.StrVal
				eps = endpointsMap[srv.Namespace+portid]
			}

			for _, ep := range eps {
				d := &ipvs.Destination{
					Address: ep.IP,
					Port: ep.Port,
					Weight: int(ep.Weight),
				}
				vs.AddDestination(d)
			}

			vss.AddVirtualServer(vs)
		}
	}

	lb.enforcer.Enforce(vss)
}

func (lb *LoadBalancer) SyncLoop(updateInterval time.Duration){
	for {
		lb.Sync()
		time.Sleep(updateInterval)
	}
}