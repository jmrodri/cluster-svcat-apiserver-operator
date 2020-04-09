package workloadcontroller

import (
	"fmt"
	"time"

	"github.com/openshift/cluster-svcat-apiserver-operator/pkg/operator/operatorclient"

	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"
	apiregistrationv1client "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	apiregistrationinformers "k8s.io/kube-aggregator/pkg/client/informers/externalversions"

	operatorsv1 "github.com/openshift/api/operator/v1"
	openshiftconfigclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	workloadDegradedCondition = "WorkloadDegraded"
	workQueueKey              = "key"
)

type ServiceCatalogAPIServerOperator struct {
	targetImagePullSpec string
	versionRecorder     status.VersionGetter

	operatorConfigClient    operatorv1client.ServiceCatalogAPIServersGetter
	openshiftConfigClient   openshiftconfigclientv1.ConfigV1Interface
	kubeClient              kubernetes.Interface
	apiregistrationv1Client apiregistrationv1client.ApiregistrationV1Interface
	eventRecorder           events.Recorder

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	rateLimiter flowcontrol.RateLimiter
}

func NewWorkloadController(
	targetImagePullSpec string,
	versionRecorder status.VersionGetter,
	operatorConfigInformer operatorv1informers.ServiceCatalogAPIServerInformer,
	kubeInformersForServiceCatalogAPIServerNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForEtcdNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForKubeAPIServerNamespace kubeinformers.SharedInformerFactory,
	kubeInformersForOpenShiftConfigNamespace kubeinformers.SharedInformerFactory,
	apiregistrationInformers apiregistrationinformers.SharedInformerFactory,
	configInformers configinformers.SharedInformerFactory,
	operatorConfigClient operatorv1client.ServiceCatalogAPIServersGetter,
	openshiftConfigClient openshiftconfigclientv1.ConfigV1Interface,
	kubeClient kubernetes.Interface,
	apiregistrationv1Client apiregistrationv1client.ApiregistrationV1Interface,
	eventRecorder events.Recorder,
) *ServiceCatalogAPIServerOperator {
	c := &ServiceCatalogAPIServerOperator{
		targetImagePullSpec:     targetImagePullSpec,
		versionRecorder:         versionRecorder,
		operatorConfigClient:    operatorConfigClient,
		openshiftConfigClient:   openshiftConfigClient,
		kubeClient:              kubeClient,
		apiregistrationv1Client: apiregistrationv1Client,
		eventRecorder:           eventRecorder,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ServiceCatalogAPIServerOperator"),

		rateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.05 /*3 per minute*/, 4),
	}

	operatorConfigInformer.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForEtcdNamespace.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForKubeAPIServerNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForServiceCatalogAPIServerNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForServiceCatalogAPIServerNamespace.Core().V1().ServiceAccounts().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForServiceCatalogAPIServerNamespace.Core().V1().Services().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForServiceCatalogAPIServerNamespace.Apps().V1().DaemonSets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenShiftConfigNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())

	// TODO: delete these
	//configInformers.Config().V1().Images().Informer().AddEventHandler(c.eventHandler())
	//apiregistrationInformers.Apiregistration().V1().APIServices().Informer().AddEventHandler(c.eventHandler())

	// we only watch some namespaces
	kubeInformersForServiceCatalogAPIServerNamespace.Core().V1().Namespaces().Informer().AddEventHandler(c.namespaceEventHandler())

	return c
}

func (c ServiceCatalogAPIServerOperator) sync() error {
	operatorConfig, err := c.operatorConfigClient.ServiceCatalogAPIServers().Get("cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}

	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
		Type:    operatorsv1.OperatorStatusTypeUpgradeable,
		Status:  operatorsv1.ConditionTrue,
		Reason:  "",
		Message: "",
	})

	switch operatorConfig.Spec.ManagementState {
	case operatorsv1.Managed:
	case operatorsv1.Unmanaged:
		originalOperatorConfig := operatorConfig.DeepCopy()
		v1helpers.RemoveOperatorCondition(&operatorConfig.Status.Conditions, removedIgnoredCondition.Type)
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
			Type:    operatorsv1.OperatorStatusTypeAvailable,
			Status:  operatorsv1.ConditionUnknown,
			Reason:  "Unmanaged",
			Message: "the apiserver is in an unmanaged state, therefore its availability is unknown.",
		})
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
			Type:    operatorsv1.OperatorStatusTypeProgressing,
			Status:  operatorsv1.ConditionFalse,
			Reason:  "Unmanaged",
			Message: "the apiserver is in an unmanaged state, therefore no changes are being applied.",
		})
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
			Type:    operatorsv1.OperatorStatusTypeDegraded,
			Status:  operatorsv1.ConditionFalse,
			Reason:  "Unmanaged",
			Message: "the apiserver is in an unmanaged state, therefore no operator actions are degraded.",
		})

		if !equality.Semantic.DeepEqual(operatorConfig.Status, originalOperatorConfig.Status) {
			if _, err := c.operatorConfigClient.ServiceCatalogAPIServers().UpdateStatus(operatorConfig); err != nil {
				return err
			}
		}
		return nil
	case operatorsv1.Removed:
		// 4.1 GA TAKE NOTICE -- don't actually take any action to uninstall.  There are no good options since we don't delete the
		// API Service and we don't want to because of secrets with ownerref & GC - see BZ https://bugzilla.redhat.com/show_bug.cgi?id=1711043
		// so don't delete the namespace.  If Admins really want to remove Service Catalog they will read the doc that says to contact support
		// (or contact support directly) and be talked through the necessity to understand the complexities of removing the aggregated API service.
		// This all will be addressed in 4.1.z ASAP.
		//
		if _, err := c.kubeClient.CoreV1().Namespaces().Get(operatorclient.TargetNamespaceName, metav1.GetOptions{}); err == nil {
			updateStatusWithRemovedIgnoredCondition(c, operatorConfig)
		} else {
			// either the cluster admin manually deleted the namespace or this is the initial deployment
			// of the operator in the cluster.  No operand namespace, set things happy.
			originalOperatorConfig := operatorConfig.DeepCopy()
			v1helpers.RemoveOperatorCondition(&operatorConfig.Status.Conditions, removedIgnoredCondition.Type)
			v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
				Type:    operatorsv1.OperatorStatusTypeAvailable,
				Status:  operatorsv1.ConditionTrue,
				Reason:  "Removed",
				Message: "the apiserver is in the desired state (Removed).",
			})
			v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
				Type:    operatorsv1.OperatorStatusTypeProgressing,
				Status:  operatorsv1.ConditionFalse,
				Reason:  "Removed",
				Message: "",
			})
			v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorsv1.OperatorCondition{
				Type:    operatorsv1.OperatorStatusTypeDegraded,
				Status:  operatorsv1.ConditionFalse,
				Reason:  "Removed",
				Message: "",
			})

			if !equality.Semantic.DeepEqual(operatorConfig.Status, originalOperatorConfig.Status) {
				if _, err := c.operatorConfigClient.ServiceCatalogAPIServers().UpdateStatus(operatorConfig); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		c.eventRecorder.Warningf("ManagementStateUnknown", "Unrecognized operator management state %q", operatorConfig.Spec.ManagementState)
		return nil
	}

	forceRequeue, err := syncServiceCatalogAPIServer_v311_00_to_latest(c, operatorConfig)
	if forceRequeue && err != nil {
		c.queue.AddRateLimited(workQueueKey)
	}

	return err
}

// Run starts the openshift-apiserver-operator and blocks until stopCh is closed.
func (c *ServiceCatalogAPIServerOperator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting OpenShiftSerCatAPIServerOperator")
	defer klog.Infof("Shutting down OpenShiftSvCatAPIServerOperator")

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *ServiceCatalogAPIServerOperator) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ServiceCatalogAPIServerOperator) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	// before we call sync, we want to wait for token.  We do this to avoid hot looping.
	c.rateLimiter.Accept()

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *ServiceCatalogAPIServerOperator) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

// this set of namespaces will include things like logging and metrics which are used to drive
var interestingNamespaces = sets.NewString(operatorclient.TargetNamespaceName)

func (c *ServiceCatalogAPIServerOperator) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				ns, ok = tombstone.Obj.(*corev1.Namespace)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace %#v", obj))
					return
				}
			}
			if ns.Name == operatorclient.TargetNamespaceName {
				c.queue.Add(workQueueKey)
			}
		},
	}
}
