package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	apigatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"
	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/constants"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/ems/api/events/types"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/env"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/handlers"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/object"
	"github.com/kyma-project/kyma/components/eventing-controller/reconciler"
	"github.com/kyma-project/kyma/components/eventing-controller/utils"
)

// Reconciler reconciles a Subscription object
type Reconciler struct {
	client.Client
	cache.Cache
	Log       logr.Logger
	recorder  record.EventRecorder
	bebClient *handlers.Beb
	Domain    string
}

var (
	Finalizer = eventingv1alpha1.GroupVersion.Group
)

const (
	suffixLength          = 10
	externalHostPrefix    = "web"
	externalSinkScheme    = "https"
	clusterLocalURLSuffix = "svc.cluster.local"
)

func NewReconciler(client client.Client, cache cache.Cache, log logr.Logger, recorder record.EventRecorder, cfg *env.Config) *Reconciler {
	bebClient := &handlers.Beb{Log: log}
	bebClient.Initialize(cfg)
	return &Reconciler{
		Client:    client,
		Cache:     cache,
		Log:       log,
		recorder:  recorder,
		bebClient: bebClient,
		Domain:    cfg.Domain,
	}
}

// +kubebuilder:rbac:groups=eventing.kyma-project.io,resources=subscriptions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eventing.kyma-project.io,resources=subscriptions/status,verbs=get;update;patch

// Generate required RBAC to emit kubernetes events in the controller
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// Source: https://book-v1.book.kubebuilder.io/beyond_basics/creating_events.html

// +kubebuilder:printcolumn:name="Ready",type=bool,JSONPath=`.status.Ready`
// Source: https://book.kubebuilder.io/reference/generating-crd.html#additional-printer-columns

// TODO: Optimize number of reconciliation calls in eventing-controller #9766: https://github.com/kyma-project/kyma/issues/9766
func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	//_ = r.Log.WithValues("subscription", req.NamespacedName)

	cachedSubscription := &eventingv1alpha1.Subscription{}

	result := ctrl.Result{}

	// Ensure the object was not deleted in the meantime
	if err := r.Client.Get(ctx, req.NamespacedName, cachedSubscription); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Handle only the new subscription
	subscription := cachedSubscription.DeepCopy()

	// Bind fields to logger
	log := r.Log.WithValues("kind", subscription.GetObjectKind().GroupVersionKind().Kind,
		"name", subscription.GetName(),
		"namespace", subscription.GetNamespace(),
		"version", subscription.GetGeneration(),
	)

	if !r.isInDeletion(subscription) {
		// Ensure the finalizer is set
		if err := r.syncFinalizer(subscription, &result, ctx, log); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to sync finalizer")
		}
		if result.Requeue {
			return result, nil
		}
		if err := r.syncInitialStatus(subscription, &result, ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to sync status")
		}
		if result.Requeue {
			return result, nil
		}
	}

	// mark if the subscription status was changed
	statusChanged := false

	// Sync with APIRule, expose the webhook
	apiRule, err := r.syncAPIRule(subscription, ctx, log)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to sync API rule")
	}
	if apiRule == nil && !r.isInDeletion(subscription) {
		log.Error(fmt.Errorf("APIRule is nil hence no host URL to work with"), "")
		// Change APIRule status to not ready
		for _, cond := range subscription.Status.Conditions {
			if cond.Type == eventingv1alpha1.ConditionAPIRuleStatus {
				if cond.Reason != eventingv1alpha1.ConditionReasonAPIRuleStatusNotReady {
					desiredSubscription := subscription.DeepCopy()
					desiredSubscription.Status.SetConditionAPIRuleStatus(false)
					desiredSubscription.Status.ExternalSink = ""
					if err := r.Status().Update(ctx, desiredSubscription); err != nil {
						return ctrl.Result{}, errors.Wrap(err, "failed to update status of Subscription when APIRule is nil")
					}
				}
				return ctrl.Result{}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	if apiRule != nil {
		apiRuleReady := computeAPIRuleReadyStatus(apiRule)

		// set initial subscription status
		subscription.Status.ExternalSink = ""
		subscription.Status.APIRuleName = apiRule.Name
		subscription.Status.SetConditionAPIRuleStatus(apiRuleReady)

		// update subscription sink only if the APIRule is ready
		if apiRuleReady {
			if err := setSubscriptionStatusExternalSink(subscription, apiRule); err != nil {
				log.Error(err, "Failed to set Subscription status externalSink", "Subscription", subscription.Name, "Namespace", subscription.Namespace)
				return ctrl.Result{}, err
			}
		}

		// update the subscription status only if needed
		if subscription.Status.APIRuleName != apiRule.Name ||
			subscription.Status.ExternalSink != cachedSubscription.Status.ExternalSink ||
			apiRuleReady != cachedSubscription.Status.GetConditionAPIRuleStatus() {
			statusChanged = true
		}
	}

	// Sync the BEB Subscription with the Subscription CR
	if statusChangedForBeb, err := r.syncBEBSubscription(subscription, &result, ctx, log, apiRule); err != nil {
		log.Error(err, "error while syncing BEB subscription")
		return ctrl.Result{}, err
	} else {
		statusChanged = statusChanged || statusChangedForBeb
	}

	if r.isInDeletion(subscription) {
		// Remove finalizers
		if err := r.removeFinalizer(subscription, ctx, log); err != nil {
			return ctrl.Result{}, err
		}
		result.Requeue = false
		return result, nil
	}

	// Save the subscription status if it was changed
	if statusChanged {
		if err := r.Status().Update(ctx, subscription); err != nil {
			log.Error(err, "Update subscription status failed")
			return ctrl.Result{}, err
		}
		result.Requeue = true
	}

	return result, nil
}

// syncFinalizer sets the finalizer in the Subscription
func (r *Reconciler) syncFinalizer(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context, logger logr.Logger) error {
	// Check if finalizer is already set
	if r.isFinalizerSet(subscription) {
		return nil
	}
	if err := r.addFinalizer(subscription, ctx, logger); err != nil {
		return err
	}
	result.Requeue = true
	return nil
}

// syncBEBSubscription delegates the subscription synchronization to the backend client. It returns true if the subscription status was changed.
func (r *Reconciler) syncBEBSubscription(subscription *eventingv1alpha1.Subscription,

	result *ctrl.Result, ctx context.Context, logger logr.Logger, apiRule *apigatewayv1alpha1.APIRule) (bool, error) {
	logger.Info("Syncing subscription with BEB")

	//No need to initialize in every sync
	//r.bebClient.Initialize()

	// if object is marked for deletion, we need to delete the BEB subscription
	if r.isInDeletion(subscription) {
		return false, r.deleteBEBSubscription(subscription, logger, ctx)
	}

	var statusChanged bool
	var err error
	if statusChanged, err = r.bebClient.SyncBebSubscription(subscription, apiRule); err != nil {
		logger.Error(err, "Update BEB subscription failed")
		condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, eventingv1alpha1.ConditionReasonSubscriptionCreationFailed, corev1.ConditionFalse)
		if err := r.updateCondition(subscription, condition, ctx); err != nil {
			return statusChanged, err
		}
		return false, err
	}

	if !subscription.Status.IsConditionSubscribed() {
		condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, eventingv1alpha1.ConditionReasonSubscriptionCreated, corev1.ConditionTrue)
		if err := r.updateCondition(subscription, condition, ctx); err != nil {
			return statusChanged, err
		}
		statusChanged = true
	}

	statusChangedAtCheck, retry, errTimeout := r.checkStatusActive(subscription)
	statusChanged = statusChanged || statusChangedAtCheck
	if errTimeout != nil {
		logger.Error(errTimeout, "timeout at retry")
		result.Requeue = false
		return statusChanged, errTimeout
	}
	if retry {
		logger.Info("Wait for subscription to be active", "name:", subscription.Name, "status:", subscription.Status.EmsSubscriptionStatus.SubscriptionStatus)
		condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscriptionActive, eventingv1alpha1.ConditionReasonSubscriptionNotActive, corev1.ConditionFalse)
		if err := r.updateCondition(subscription, condition, ctx); err != nil {
			return statusChanged, err
		}
		result.RequeueAfter = time.Second * 1
	} else if statusChanged {
		condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscriptionActive, eventingv1alpha1.ConditionReasonSubscriptionActive, corev1.ConditionTrue)
		if err := r.updateCondition(subscription, condition, ctx); err != nil {
			return statusChanged, err
		}
	}
	// OK
	return statusChanged, nil
}

// deleteBEBSubscription deletes the BEB subscription and updates the condition and k8s events
func (r *Reconciler) deleteBEBSubscription(subscription *eventingv1alpha1.Subscription, logger logr.Logger, ctx context.Context) error {
	logger.Info("Deleting BEB subscription")
	if err := r.bebClient.DeleteBebSubscription(subscription); err != nil {
		return err
	}
	condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, eventingv1alpha1.ConditionReasonSubscriptionDeleted, corev1.ConditionFalse)
	return r.updateCondition(subscription, condition, ctx)
}

func (r *Reconciler) syncAPIRule(subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) (*apigatewayv1alpha1.APIRule, error) {
	if subscription.DeletionTimestamp != nil {
		logger.Info("subscription is getting deleted so nothing needs to be done")
		return nil, nil
	}

	isValidSink, err := r.isSinkURLValid(ctx, subscription, logger)
	if err != nil {
		logger.Error(err, "failed to validate sink URLs")
		return nil, err
	}
	if !isValidSink {
		logger.Error(fmt.Errorf("sink URL is not valid"), subscription.Spec.Sink)
		return nil, nil
	}

	sURL, err := url.ParseRequestURI(subscription.Spec.Sink)
	if err != nil {
		logger.Error(err, "failed to parse sink URI")
		r.eventWarn(subscription, reasonValidationFailed, "Failed to parse sink URI %s", subscription.Spec.Sink)
		return nil, nil
	}
	apiRule, err := r.createOrUpdateAPIRule(subscription, ctx, *sURL, logger)
	if err != nil {
		return nil, errors.Wrap(err, "failed to createOrUpdateAPIRule")
	}
	return apiRule, nil
}

func (r *Reconciler) isSinkURLValid(ctx context.Context, subscription *eventingv1alpha1.Subscription, logger logr.Logger) (bool, error) {
	if !isValidScheme(subscription.Spec.Sink) {
		r.eventWarn(subscription, reasonValidationFailed, "Sink URL scheme should be 'http' or 'https' %s", subscription.Spec.Sink)
		return false, nil
	}

	sURL, err := url.ParseRequestURI(subscription.Spec.Sink)
	if err != nil {
		logger.Error(err, subscription.Spec.Sink)
		r.eventWarn(subscription, reasonValidationFailed, "Sink URL is not valid %s", err.Error())
		return false, nil
	}

	// Validate sink URL is a cluster local URL
	trimmedHost := strings.Split(sURL.Host, ":")[0]
	if !strings.HasSuffix(trimmedHost, clusterLocalURLSuffix) {
		logger.Error(fmt.Errorf("sink does not contain suffix: %s in the URL", clusterLocalURLSuffix), "")
		r.eventWarn(subscription, reasonValidationFailed, "sink does not contain suffix: %s in the URL", clusterLocalURLSuffix)
		return false, nil
	}
	subDomains := strings.Split(trimmedHost, ".")
	if len(subDomains) != 5 {
		logger.Error(fmt.Errorf("sink should contain 5 sub-domains"), trimmedHost)
		r.eventWarn(subscription, reasonValidationFailed, "sink should contain 5 sub-domains %s", trimmedHost)
		return false, nil
	}

	svcNs, svcName := subDomains[1], subDomains[0]
	// Assumption: Subscription CR and Subscriber should be deployed in the same namespace
	if subscription.Namespace != svcNs {
		logger.Error(fmt.Errorf("the namespace of Subscription: %s and the namespace of subscriber: %s are different", subscription.Namespace, svcNs), "")
		r.eventWarn(subscription, reasonValidationFailed, "the namespace of Subscription: %s and the namespace of subscriber: %s are different", subscription.Namespace, svcNs)
		return false, nil
	}

	// Validate svc is a cluster-local one
	_, err = r.getClusterLocalService(ctx, svcNs, svcName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Error(err, "sink doesn't correspond to a valid cluster local svc")
			r.eventWarn(subscription, reasonValidationFailed, "sink doesn't correspond to a valid cluster local svc")
			return false, nil
		}
		r.eventWarn(subscription, reasonValidationFailed, "failed to fetch cluster-local svc %s/%s", svcNs, svcName)
		return false, errors.Wrapf(err, "failed to fetch cluster-local svc %s/%s", svcNs, svcName)
	}
	return true, nil
}

func (r *Reconciler) getClusterLocalService(ctx context.Context, svcNs, svcName string) (*corev1.Service, error) {
	svcLookupKey := k8stypes.NamespacedName{Name: svcName, Namespace: svcNs}
	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, svcLookupKey, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

func (r *Reconciler) createOrUpdateAPIRule(subscription *eventingv1alpha1.Subscription, ctx context.Context, sink url.URL, logger logr.Logger) (*apigatewayv1alpha1.APIRule, error) {
	svcNs, svcName, err := getSvcNsAndName(sink.Host)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse svc name and ns in createOrUpdateAPIRule")
	}
	labels := map[string]string{
		constants.ControllerServiceLabelKey:  svcName,
		constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
	}

	svcPort, err := utils.GetPortNumberFromURL(sink)
	if err != nil {
		return nil, errors.Wrap(err, "failed to convert URL port to APIRule port")
	}
	var existingAPIRule *apigatewayv1alpha1.APIRule
	existingAPIRules, err := r.getAPIRulesForASvc(ctx, labels, svcNs)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch existing ApiRule for labels: %v", labels)
	}
	if existingAPIRules != nil {
		existingAPIRule = r.filterAPIRulesOnPort(existingAPIRules, svcPort)
	}

	// Get all subscriptions valid for the cluster-local subscriber
	subscriptions, err := r.getSubscriptionsForASvc(svcNs, svcName, ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch subscriptions for the subscriber %s/%s", svcNs, svcName)
	}
	filteredSubscriptions := r.filterSubscriptionsOnPort(subscriptions, svcPort)

	desiredAPIRule, err := r.makeAPIRule(svcNs, svcName, labels, filteredSubscriptions, svcPort)
	if err != nil {
		return nil, errors.Wrap(err, "failed to make an APIRule")
	}

	if existingAPIRule == nil {
		err = r.Client.Create(ctx, desiredAPIRule, &client.CreateOptions{})
		if err != nil {
			r.eventWarn(subscription, reasonCreateFailed, "Create APIRule failed %s", desiredAPIRule.Name)
			return nil, errors.Wrap(err, "failed to create APIRule")
		}
		r.eventNormal(subscription, reasonCreate, "Created APIRule %s", desiredAPIRule.Name)
		return desiredAPIRule, nil
	}
	logger.Info("Existing APIRules", fmt.Sprintf("in ns: %s for svc: %s", svcNs, svcName), fmt.Sprintf("%s", existingAPIRule.Name))

	object.ApplyExistingAPIRuleAttributes(existingAPIRule, desiredAPIRule)
	if object.Semantic.DeepEqual(existingAPIRule, desiredAPIRule) {
		return existingAPIRule, nil
	}
	err = r.Client.Update(ctx, desiredAPIRule, &client.UpdateOptions{})
	if err != nil {
		r.eventWarn(subscription, reasonUpdateFailed, "Update APIRule failed %s", desiredAPIRule.Name)
		return nil, errors.Wrap(err, "failed to update an APIRule")
	}
	r.eventNormal(subscription, reasonUpdate, "Updated APIRule %s", desiredAPIRule.Name)

	freshExistingAPIRules, err := r.getAPIRulesForASvc(ctx, labels, svcNs)
	if err != nil {
		return nil, errors.Wrapf(err, "error while fetching oldApiRules for labels: %v after create/update", labels)
	}
	// Cleanup does the following:
	// 1. Delete APIRule using obsolete ports
	// 2. Update APIRule by deleting the OwnerReference of the Subscription with port different than that of the APIRule
	err = r.cleanup(subscription, ctx, subscriptions, freshExistingAPIRules)
	if err != nil {
		return nil, errors.Wrap(err, "failed to cleanup APIRules")
	}
	return desiredAPIRule, nil
}

func (r *Reconciler) cleanup(subscription *eventingv1alpha1.Subscription, ctx context.Context, subs []eventingv1alpha1.Subscription, apiRules []apigatewayv1alpha1.APIRule) error {
	for _, apiRule := range apiRules {
		filteredOwnerRefs := make([]metav1.OwnerReference, 0)
		for _, or := range apiRule.OwnerReferences {
			for _, sub := range subs {
				if isOwnerRefBelongingToSubscription(sub, or) {
					subSinkURL, err := url.ParseRequestURI(sub.Spec.Sink)
					if err != nil {
						// It's ok as this subscription doesn't have a port anyway
						continue
					}
					port, err := utils.GetPortNumberFromURL(*subSinkURL)
					if err != nil {
						// It's ok as the port is not valid anyway
						continue
					}
					if port == *apiRule.Spec.Service.Port {
						filteredOwnerRefs = append(filteredOwnerRefs, or)
					}
				}
			}
		}

		// TODO the logic below introduce flakiness in the tests, uncomment and fix it
		// Delete the APIRule as the port for the concerned svc is not used by any subscriptions
		//if len(filteredOwnerRefs) == 0 {
		//	err := r.Client.Delete(ctx, &apiRule, &client.DeleteOptions{})
		//	if err != nil {
		//		r.eventWarn(subscription, reasonDeleteFailed, "Deleted APIRule failed %s", apiRule.Name)
		//		return errors.Wrap(err, "failed to delete APIRule while cleanupAPIRules")
		//	}
		//	r.eventNormal(subscription, reasonDelete, "Deleted APIRule %s", apiRule.Name)
		//	return nil
		//}

		// Take the subscription out of the OwnerReferences and update the APIRule
		desiredAPIRule := apiRule.DeepCopy()
		object.ApplyExistingAPIRuleAttributes(&apiRule, desiredAPIRule)
		desiredAPIRule.OwnerReferences = filteredOwnerRefs
		err := r.Client.Update(ctx, desiredAPIRule, &client.UpdateOptions{})
		if err != nil {
			r.eventWarn(subscription, reasonUpdateFailed, "Update APIRule failed %s", apiRule.Name)
			return errors.Wrap(err, "failed to update APIRule while cleanupAPIRules")
		}
		r.eventNormal(subscription, reasonUpdate, "Updated APIRule %s", apiRule.Name)
		return nil
	}
	return nil
}

func isOwnerRefBelongingToSubscription(sub eventingv1alpha1.Subscription, ownerRef metav1.OwnerReference) bool {
	if sub.Name == ownerRef.Name && sub.UID == ownerRef.UID {
		return true
	}
	return false
}

// getSubscriptionsForASvc returns a list of Subscriptions which are valid for the subscriber in focus
func (r *Reconciler) getSubscriptionsForASvc(svcNs, svcName string, ctx context.Context) ([]eventingv1alpha1.Subscription, error) {
	subscriptions := &eventingv1alpha1.SubscriptionList{}
	relevantSubs := make([]eventingv1alpha1.Subscription, 0)
	err := r.Client.List(ctx, subscriptions, &client.ListOptions{
		Namespace: svcNs,
	})
	if err != nil {
		return []eventingv1alpha1.Subscription{}, err
	}
	for _, sub := range subscriptions.Items {
		// Filtering subscriptions which are being deleted at the moment
		if sub.DeletionTimestamp != nil {
			continue
		}
		hostURL, err := url.ParseRequestURI(sub.Spec.Sink)
		if err != nil {
			// It's ok as the relevant subscription will have a valid cluster local URL in the same namespace
			continue
		}
		// Filtering subscriptions valid for a valid subscriber
		svcNsForSub, svcNameForSub, err := getSvcNsAndName(hostURL.Host)
		if err != nil {
			// It's ok as the relevant subscription will have a valid cluster local URL in the same namespace
			continue
		}
		//svcPortForSub, err := convertURLPortForApiRulePort(*hostURL)
		if svcNs == svcNsForSub && svcName == svcNameForSub {
			relevantSubs = append(relevantSubs, sub)
		}
	}
	return relevantSubs, nil
}

// filterSubscriptionsOnPort returns a list of Subscriptions which matches a particular port
func (r *Reconciler) filterSubscriptionsOnPort(subList []eventingv1alpha1.Subscription, svcPort uint32) []eventingv1alpha1.Subscription {
	filteredSubs := make([]eventingv1alpha1.Subscription, 0)
	for _, sub := range subList {
		// Filtering subscriptions which are being deleted at the moment
		if sub.DeletionTimestamp != nil {
			continue
		}
		hostURL, err := url.ParseRequestURI(sub.Spec.Sink)
		if err != nil {
			// It's ok as the relevant subscription will have a valid cluster local URL in the same namespace
			continue
		}

		svcPortForSub, err := utils.GetPortNumberFromURL(*hostURL)
		if err != nil {
			// It's ok as the relevant subscription will have a valid port to filter on
			continue
		}
		if svcPort == svcPortForSub {
			filteredSubs = append(filteredSubs, sub)
		}
	}
	return filteredSubs
}

func (r *Reconciler) makeAPIRule(svcNs, svcName string, labels map[string]string, subs []eventingv1alpha1.Subscription, port uint32) (*apigatewayv1alpha1.APIRule, error) {

	randomSuffix := handlers.GetRandString(suffixLength)
	hostName := fmt.Sprintf("%s-%s.%s", externalHostPrefix, randomSuffix, r.Domain)

	apiRule := object.NewAPIRule(svcNs, reconciler.ApiRuleNamePrefix,
		object.WithLabels(labels),
		object.WithOwnerReference(subs),
		object.WithService(hostName, svcName, port),
		object.WithGateway(constants.ClusterLocalAPIGateway),
		object.WithRules(subs, http.MethodPost, http.MethodOptions))
	return apiRule, nil
}

func (r *Reconciler) getAPIRulesForASvc(ctx context.Context, labels map[string]string, svcNs string) ([]apigatewayv1alpha1.APIRule, error) {
	existingAPIRules := &apigatewayv1alpha1.APIRuleList{}
	err := r.Client.List(ctx, existingAPIRules, &client.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(labels),
		Namespace:     svcNs,
	})
	if err != nil {
		return nil, err
	}
	return existingAPIRules.Items, nil
}

func (r *Reconciler) filterAPIRulesOnPort(existingAPIRules []apigatewayv1alpha1.APIRule, port uint32) *apigatewayv1alpha1.APIRule {
	// Assumption: there will be one APIRule for an svc with the labels injected by the controller hence trusting the first match
	for _, apiRule := range existingAPIRules {
		if *apiRule.Spec.Service.Port == port {
			return &apiRule
		}
	}
	return nil
}

// getSvcNsAndName returns namespace and name of the svc from the URL
func getSvcNsAndName(url string) (string, string, error) {
	parts := strings.Split(url, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid sinkURL for cluster local svc: %s", url)
	}
	return parts[1], parts[0], nil
}

// syncInitialStatus determines the desires initial status and updates it accordingly (if conditions changed)
func (r *Reconciler) syncInitialStatus(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context) error {
	currentStatus := subscription.Status
	expectedStatus := eventingv1alpha1.SubscriptionStatus{}
	expectedStatus.InitializeConditions()
	currentReadyStatusFromConditions := currentStatus.IsReady()

	var updateReadyStatus bool
	if currentReadyStatusFromConditions && !currentStatus.Ready {
		currentStatus.Ready = true
		updateReadyStatus = true
	} else if !currentReadyStatusFromConditions && currentStatus.Ready {
		currentStatus.Ready = false
		updateReadyStatus = true
	}
	// case: conditions are already initialized
	if len(currentStatus.Conditions) >= len(expectedStatus.Conditions) && !updateReadyStatus {
		return nil
	}
	if len(currentStatus.Conditions) == 0 {
		subscription.Status = expectedStatus
	} else {
		subscription.Status.Ready = currentStatus.Ready
	}
	if err := r.Status().Update(ctx, subscription); err != nil {
		return err
	}
	result.Requeue = true
	return nil
}

// updateCondition replaces the given condition on the subscription and updates the status as well as emitting a kubernetes event
func (r *Reconciler) updateCondition(subscription *eventingv1alpha1.Subscription, condition eventingv1alpha1.Condition, ctx context.Context) error {
	needsUpdate, err := r.replaceStatusCondition(subscription, condition)
	if err != nil {
		return err
	}
	if !needsUpdate {
		return nil
	}

	if err := r.Status().Update(ctx, subscription); err != nil {
		return err
	}

	r.emitConditionEvent(subscription, condition)
	return nil
}

// replaceStatusCondition replaces the given condition on the subscription. Also it sets the readiness in the status.
// So make sure you always use this method then changing a condition
func (r *Reconciler) replaceStatusCondition(subscription *eventingv1alpha1.Subscription, condition eventingv1alpha1.Condition) (bool, error) {
	// the subscription is ready if all conditions are fulfilled
	isReady := true

	// compile list of desired conditions
	desiredConditions := make([]eventingv1alpha1.Condition, 0)
	for _, c := range subscription.Status.Conditions {
		var chosenCondition eventingv1alpha1.Condition
		if c.Type == condition.Type {
			// take given condition
			chosenCondition = condition
		} else {
			// take already present condition
			chosenCondition = c
		}
		desiredConditions = append(desiredConditions, chosenCondition)
		if string(chosenCondition.Status) != string(v1.ConditionTrue) {
			isReady = false
		}
	}

	// prevent unnecessary updates
	if conditionsEquals(subscription.Status.Conditions, desiredConditions) && subscription.Status.Ready == isReady {
		return false, nil
	}

	// update the status
	subscription.Status.Conditions = desiredConditions
	subscription.Status.Ready = isReady
	return true, nil
}

// emitConditionEvent emits a kubernetes event and sets the event type based on the Condition status
func (r *Reconciler) emitConditionEvent(subscription *eventingv1alpha1.Subscription, condition eventingv1alpha1.Condition) {
	eventType := corev1.EventTypeNormal
	if condition.Status == corev1.ConditionFalse {
		eventType = corev1.EventTypeWarning
	}
	r.recorder.Event(subscription, eventType, string(condition.Reason), condition.Message)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&eventingv1alpha1.Subscription{}).
		Watches(&source.Kind{Type: &apigatewayv1alpha1.APIRule{}}, r.getAPIRuleEventHandler()).
		Complete(r)
}

// getAPIRuleEventHandler returns an APIRule event handler.
func (r *Reconciler) getAPIRuleEventHandler() handler.EventHandler {
	eventHandler := func(eventType, name, namespace string, q workqueue.RateLimitingInterface) {
		log := r.Log.WithValues("event", eventType, "kind", "APIRule", "name", name, "namespace", namespace)
		if err := r.handleAPIRuleEvent(name, namespace, q, log); err != nil {
			log.Error(err, "Failed to handle APIRule Event, requeue event again")
			q.Add(reconcile.Request{NamespacedName: k8stypes.NamespacedName{Name: name, Namespace: namespace}})
		}
	}

	return handler.Funcs{
		CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
			eventType, name, namespace := "Create", e.Meta.GetName(), e.Meta.GetNamespace()
			eventHandler(eventType, name, namespace, q)
		},
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			eventType, name, namespace := "Update", e.MetaNew.GetName(), e.MetaNew.GetNamespace()
			eventHandler(eventType, name, namespace, q)
		},
		DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
			eventType, name, namespace := "Delete", e.Meta.GetName(), e.Meta.GetNamespace()
			eventHandler(eventType, name, namespace, q)
		},
		GenericFunc: func(e event.GenericEvent, q workqueue.RateLimitingInterface) {
			eventType, name, namespace := "Generic", e.Meta.GetName(), e.Meta.GetNamespace()
			eventHandler(eventType, name, namespace, q)
		},
	}
}

// handleAPIRuleEvent handles APIRule event.
func (r *Reconciler) handleAPIRuleEvent(name, namespace string, q workqueue.RateLimitingInterface, log logr.Logger) error {
	// skip not relevant APIRules
	if !isRelevantAPIRuleName(name) {
		return nil
	}

	log.Info("Handle APIRule Event")

	// try to get the APIRule from the API server
	ctx := context.Background()
	apiRule := &apigatewayv1alpha1.APIRule{}
	key := k8stypes.NamespacedName{Name: name, Namespace: namespace}
	if err := r.Client.Get(ctx, key, apiRule); err != nil {
		log.Info("Cannot get APIRule")
	}

	// list all namespace subscriptions
	namespaceSubscriptions := &eventingv1alpha1.SubscriptionList{}
	if err := r.Client.List(ctx, namespaceSubscriptions, client.InNamespace(namespace)); err != nil {
		log.Error(err, "Failed to list namespace Subscriptions")
		return err
	}

	// filter namespace subscriptions that are relevant to the current APIRule
	apiRuleSubscriptions := make([]eventingv1alpha1.Subscription, 0, len(apiRule.ObjectMeta.OwnerReferences))
	for _, subscription := range namespaceSubscriptions.Items {
		// skip if the subscription is marked for deletion
		if subscription.DeletionTimestamp != nil {
			continue
		}

		// check if APIRule name match
		if subscription.Status.APIRuleName == name {
			apiRuleSubscriptions = append(apiRuleSubscriptions, subscription)
			continue
		}

		// check if APIRule OwnerReferences contains subscription info
		if containsOwnerReference(apiRule.ObjectMeta.OwnerReferences, subscription.UID) {
			apiRuleSubscriptions = append(apiRuleSubscriptions, subscription)
			continue
		}
	}

	// queue reconcile requests for APIRule subscriptions
	r.queueReconcileRequestForSubscriptions(apiRuleSubscriptions, q, log)

	return nil
}

// containsOwnerReference returns true if the OwnerReferences list contains the given uid, otherwise returns false.
func containsOwnerReference(ownerReferences []v1.OwnerReference, uid k8stypes.UID) bool {
	for _, ownerReference := range ownerReferences {
		if ownerReference.UID == uid {
			return true
		}
	}
	return false
}

// queueReconcileRequestForSubscriptions queues reconciliation requests for the given subscriptions.
func (r *Reconciler) queueReconcileRequestForSubscriptions(subscriptions []eventingv1alpha1.Subscription, q workqueue.RateLimitingInterface, log logr.Logger) {
	subscriptionNames := make([]string, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		request := reconcile.Request{
			NamespacedName: k8stypes.NamespacedName{
				Name:      subscription.Name,
				Namespace: subscription.Namespace,
			},
		}
		q.Add(request)
		subscriptionNames = append(subscriptionNames, subscription.Name)
	}
	log.Info("Queue Subscription Reconcile Requests", "Subscriptions", subscriptionNames)
}

// isRelevantAPIRuleName returns true if the given name matches the APIRule name pattern
// used by the eventing-controller, otherwise returns false.
func isRelevantAPIRuleName(name string) bool {
	return strings.HasPrefix(name, reconciler.ApiRuleNamePrefix)
}

// computeAPIRuleReadyStatus returns true if all APIRule statuses is ok, otherwise returns false.
func computeAPIRuleReadyStatus(apiRule *apigatewayv1alpha1.APIRule) bool {
	if apiRule.Status.APIRuleStatus == nil || apiRule.Status.AccessRuleStatus == nil || apiRule.Status.VirtualServiceStatus == nil {
		return false
	}
	apiRuleStatus := apiRule.Status.APIRuleStatus.Code == apigatewayv1alpha1.StatusOK
	accessRuleStatus := apiRule.Status.AccessRuleStatus.Code == apigatewayv1alpha1.StatusOK
	virtualServiceStatus := apiRule.Status.VirtualServiceStatus.Code == apigatewayv1alpha1.StatusOK
	return apiRuleStatus && accessRuleStatus && virtualServiceStatus
}

// setSubscriptionStatusExternalSink sets the subscription external sink based on the given APIRule service host.
func setSubscriptionStatusExternalSink(subscription *eventingv1alpha1.Subscription, apiRule *apigatewayv1alpha1.APIRule) error {
	if apiRule.Spec.Service == nil {
		return errors.Errorf("APIRule has nil service")
	}

	if apiRule.Spec.Service.Host == nil {
		return errors.Errorf("APIRule has nil host")
	}

	u, err := url.ParseRequestURI(subscription.Spec.Sink)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf(fmt.Sprintf("subscription: [%s/%s] has invalid sink", subscription.Name, subscription.Namespace)))
	}

	path := u.Path
	if u.Path == "" {
		path = "/"
	}

	subscription.Status.ExternalSink = fmt.Sprintf("%s://%s%s", externalSinkScheme, *apiRule.Spec.Service.Host, path)

	return nil
}

func (r *Reconciler) addFinalizer(subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) error {
	subscription.ObjectMeta.Finalizers = append(subscription.ObjectMeta.Finalizers, Finalizer)
	logger.V(1).Info("Adding finalizer")
	if err := r.Update(ctx, subscription); err != nil {
		return errors.Wrapf(err, "error while adding Finalizer with name: %s", Finalizer)
	}
	logger.V(1).Info("Added finalizer")
	return nil
}

func (r *Reconciler) removeFinalizer(subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) error {
	var finalizers []string

	// Build finalizer list without the one the controller owns
	for _, finalizer := range subscription.ObjectMeta.Finalizers {
		if finalizer == Finalizer {
			continue
		}
		finalizers = append(finalizers, finalizer)
	}

	logger.V(1).Info("Removing finalizer")
	subscription.ObjectMeta.Finalizers = finalizers
	if err := r.Update(ctx, subscription); err != nil {
		return errors.Wrapf(err, "error while removing Finalizer with name: %s", Finalizer)
	}
	logger.V(1).Info("Removed finalizer")
	return nil
}

// isFinalizerSet checks if a finalizer is set on the Subscription which belongs to this controller
func (r *Reconciler) isFinalizerSet(subscription *eventingv1alpha1.Subscription) bool {
	// Check if finalizer is already set
	for _, finalizer := range subscription.ObjectMeta.Finalizers {
		if finalizer == Finalizer {
			return true
		}
	}
	return false
}

// isInDeletion checks if the Subscription shall be deleted
func (r *Reconciler) isInDeletion(subscription *eventingv1alpha1.Subscription) bool {
	return !subscription.DeletionTimestamp.IsZero()
}

const timeoutRetryActiveEmsStatus = time.Second * 30

// checkStatusActive checks if the subscription is active and if not, sets a timer for retry
func (r *Reconciler) checkStatusActive(subscription *eventingv1alpha1.Subscription) (statusChanged, retry bool, err error) {
	if subscription.Status.EmsSubscriptionStatus.SubscriptionStatus == string(types.SubscriptionStatusActive) {
		if len(subscription.Status.FailedActivation) > 0 {
			subscription.Status.FailedActivation = ""
			return true, false, nil
		}
		return false, false, nil
	}
	t1 := time.Now()
	if len(subscription.Status.FailedActivation) == 0 {
		// it's the first time
		subscription.Status.FailedActivation = t1.Format(time.RFC3339)
		return true, true, nil
	}
	// check the timeout
	if t0, er := time.Parse(time.RFC3339, subscription.Status.FailedActivation); er != nil {
		err = er
	} else if t1.Sub(t0) > timeoutRetryActiveEmsStatus {
		err = fmt.Errorf("timeout waiting for the subscription to be active: %v", subscription.Name)
	} else {
		retry = true
	}
	return false, retry, err
}

// isValidScheme returns true if the sink scheme is http or https, otherwise returns false.
func isValidScheme(sink string) bool {
	return strings.HasPrefix(sink, "http://") || strings.HasPrefix(sink, "https://")
}
