package configobservation

import (
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	corelistersv1 "k8s.io/client-go/listers/core/v1"
)

type Listers struct {
	ResourceSync resourcesynccontroller.ResourceSyncer

	//APIServerLister_    configlistersv1.APIServerLister
	//ImageConfigLister   configlistersv1.ImageLister
	//ProjectConfigLister configlistersv1.ProjectLister
	//ProxyLister_        configlistersv1.ProxyLister
	//IngressConfigLister configlistersv1.IngressLister
	EndpointsLister corelistersv1.EndpointsLister
	//PreRunCachesSynced  []cache.InformerSynced
	//SecretLister_       corelistersv1.SecretLister
}

/*
func (l Listers) ResourceSyncer() resourcesynccontroller.ResourceSyncer {
	return l.ResourceSync
}

func (l Listers) SecretLister() corelistersv1.SecretLister {
	return l.SecretLister_
}

func (l Listers) PreRunHasSynced() []cache.InformerSynced {
	return l.PreRunCachesSynced
}

func (l Listers) APIServerLister() configlistersv1.APIServerLister {
	return l.APIServerLister_
}

func (l Listers) ProxyLister() configlistersv1.ProxyLister {
	return l.ProxyLister_
}
*/
