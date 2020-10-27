package controllers

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/util/rand"

	k8slabels "k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/go-logr/logr"
	types2 "github.com/kyma-project/kyma/components/eventing-controller/pkg/ems2/api/events/types"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/handlers"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/object"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	// TODO: use different package
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apigatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"
	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
)

// SubscriptionReconciler reconciles a Subscription object
type SubscriptionReconciler struct {
	client.Client
	cache.Cache
	Log       logr.Logger
	recorder  record.EventRecorder
	Scheme    *runtime.Scheme
	bebClient *handlers.Beb
}

// TODO: emit events
// TODO: use additional printer columns: https://book.kubebuilder.io/reference/generating-crd.html#additional-printer-columns

var (
	FinalizerName = eventingv1alpha1.GroupVersion.Group
)

const (
	SinkURLPrefix                = "webhook"
	SuffixLength                 = 6
	ClusterLocalAPIGateway       = "kyma-gateway.kyma-system.svc.cluster.local"
	ControllerServiceLabelKey    = "service"
	ControllerIdentityLabelKey   = "beb"
	ControllerIdentityLabelValue = "webhook"
)

func NewSubscriptionReconciler(
	client client.Client,
	cache cache.Cache,
	log logr.Logger,
	recorder record.EventRecorder,
	scheme *runtime.Scheme,
) *SubscriptionReconciler {
	bebClient := &handlers.Beb{
		Log: log,
	}
	return &SubscriptionReconciler{
		Client:    client,
		Cache:     cache,
		Log:       log,
		recorder:  recorder,
		Scheme:    scheme,
		bebClient: bebClient,
	}
}

// +kubebuilder:rbac:groups=eventing.kyma-project.io,resources=subscriptions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eventing.kyma-project.io,resources=subscriptions/status,verbs=get;update;patch

// Generate required RBAC to emit kubernetes events in the controller
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// Source: https://book-v1.book.kubebuilder.io/beyond_basics/creating_events.html

func (r *SubscriptionReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("subscription", req.NamespacedName)

	cachedSubscription := &eventingv1alpha1.Subscription{}

	ctx := context.TODO()
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
			log.Error(err, "error while syncing finalizer")
			return result, err
		}
		if result.Requeue {
			return result, nil
		}
		if err := r.syncInitialStatus(subscription, &result, ctx); err != nil {
			log.Error(err, "error while syncing status")
			return result, err
		}
		if result.Requeue {
			return result, nil
		}
	}

	// Sync with APIRule, expose the webhook
	if err := r.syncAPIRule(subscription, &result, ctx, log); err != nil {
		log.Error(err, "error while syncing API rule")
		return result, err
	}

	//// Sync the BEB Subscription with the Subscription CR
	//if err := r.syncBEBSubscription(subscription, &result, ctx, log); err != nil {
	//	log.Error(err, "error while syncing BEB subscription")
	//	return result, err
	//}

	if r.isInDeletion(subscription) {
		// Remove finalizers
		if err := r.removeFinalizer(subscription, ctx, log); err != nil {
			return result, err
		}
		result.Requeue = false
	}
	return result, nil
}

// syncFinalizer sets the finalizer in the Subscription
func (r *SubscriptionReconciler) syncFinalizer(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context, logger logr.Logger) error {
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

func (r *SubscriptionReconciler) syncBEBSubscription(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context, logger logr.Logger) error {
	logger.Info("Syncing subscription with BEB")
	// TODO: get beb credentials from secret

	r.bebClient.Initialize()

	if r.isInDeletion(subscription) {
		logger.Info("Deleting BEB subscription")
		if err := r.bebClient.DeleteBebSubscription(subscription); err != nil {
			return err
		}
		return nil
	}

	var statusChanged bool
	var err error
	if statusChanged, err = r.bebClient.SyncBebSubscription(subscription); err != nil {
		logger.Error(err, "Update BEB subscription failed")
		return err
	}

	condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, "Successfully synchronized with BEB subscription", corev1.ConditionTrue)
	if !subscription.Status.IsConditionSubscribed() {
		if err := r.replaceStatusCondition(subscription, condition); err != nil {
			return err
		}
		statusChanged = true
	}

	statusChangedAtCheck, retry, errTimeout := r.checkStatusActive(subscription)
	statusChanged = statusChanged || statusChangedAtCheck
	// save the new status only if it was changed
	if statusChanged {
		if err := r.Status().Update(ctx, subscription); err != nil {
			logger.Error(err, "Update subscription status failed")
			return err
		}
		result.Requeue = true
	}
	if errTimeout != nil {
		logger.Error(errTimeout, "Timeout at retry")
		result.Requeue = false
		return errTimeout
	}
	if retry {
		logger.Info("Wait for subscription to be active", "name:", subscription.Name, "status:", subscription.Status.EmsSubscriptionStatus.SubscriptionStatus)
		warn := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, "Subscription not active", corev1.ConditionFalse)
		result.RequeueAfter = time.Second * 1
		r.emitConditionEvent(subscription, warn, "Wait for subscription to be active", "Retry")
	} else if statusChanged {
		r.emitConditionEvent(subscription, condition, "Subscription active", "")
	}
	// OK
	return nil
}

func (r *SubscriptionReconciler) syncAPIRule(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context, logger logr.Logger) error {
	// Validate correctness of a URL
	sURL, err := url.ParseRequestURI(subscription.Spec.Sink)
	if err != nil {
		logger.Error(err, "url is invalid")
		return nil
	}

	// Validate svcNs and svcName from sink URL
	svcNs, svcName, err := getSvcNsAndName(sURL.Host)
	if err != nil {
		logger.Error(err, "failed to parse svcName and svcNamespace")
		return nil
	}

	// Assumption: Subscription CR and Subscriber should be deployed in the same namespace
	if subscription.Namespace != svcNs {
		logger.Error(fmt.Errorf("stopping reconciliation as the namespace of Subscription: %s and the namespace of subscriber: %s are different", subscription.Namespace, svcNs), "")
		return nil
	}

	// Validate svc is a cluster local one
	_, err = r.validateClusterLocalService(ctx, svcNs, svcName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Error(err, "sink doesn't correspond to a valid cluster local svc")
		}
		return errors.Wrap(err, "failed to get the svc")
	}

	err = r.createOrUpdateAPIRule(*sURL, subscription, ctx, logger)
	if err != nil {
		return errors.Wrap(err, "failed to createOrUpdateAPIRule")
	}
	return nil
}

func (r *SubscriptionReconciler) validateClusterLocalService(ctx context.Context, svcNs, svcName string) (*corev1.Service, error) {
	svcLookupKey := types.NamespacedName{Name: svcName, Namespace: svcNs}
	svc := &corev1.Service{}
	if err := r.Cache.Get(ctx, svcLookupKey, svc); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, errors.Wrap(err, "sink does not correspond to a valid k8s svc")
		}
		return nil, err
	}
	return svc, nil
}

// getSvcNsAndName returns namespace and name of the svc from the URL
func getSvcNsAndName(url string) (string, string, error) {
	parts := strings.Split(url, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid sinkURL for cluster local svc: %s", url)
	}
	return parts[1], parts[0], nil
}

func (r *SubscriptionReconciler) createOrUpdateAPIRule(sink url.URL, subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) error {
	svcNs, svcName, err := getSvcNsAndName(sink.Host)
	if err != nil {
		return errors.Wrap(err, "failed to parse svc name and ns in createOrUpdateAPIRule")
	}
	labels := map[string]string{
		ControllerServiceLabelKey:  svcName,
		ControllerIdentityLabelKey: ControllerIdentityLabelValue,
	}
	existingAPIRules := &apigatewayv1alpha1.APIRuleList{}
	err = r.Cache.List(ctx, existingAPIRules, &client.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(labels),
		Namespace:     svcNs,
	})
	if err != nil {
		logger.Error(err, "error while fetching oldApiRule for labels", labels)
		return nil
	}
	logger.Info("Existing APIRules", fmt.Sprintf("in ns: %s for svc: %s", svcNs, svcName), existingAPIRules.Items)

	// Get all subscriptions valid for the cluster-local subscriber
	subscriptions, err := r.getRelevantSubscriptions(svcNs, svcName, ctx)
	if err != nil {
		logger.Error(err, "failed to fetch subscriptions for the subscriber is focus")
		return nil
	}

	desiredAPIRule, err := r.makeAPIRule(svcNs, svcName, labels, subscriptions, sink)
	if err != nil {
		return errors.Wrap(err, "failed to make an APIRule")
	}
	if len(existingAPIRules.Items) < 1 {
		err = r.Client.Create(ctx, desiredAPIRule, &client.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create APIRule")
		}
		return nil
	}
	// Assumption: there will be one APIRule for an svc with the labels injected by the controller hence trusting the 0th element in existingAPIRules list
	existingAPIRule := &existingAPIRules.Items[0]
	object.ApplyExistingAPIRuleAttributes(existingAPIRule, desiredAPIRule)
	if object.Semantic.DeepEqual(existingAPIRule, desiredAPIRule) {
		return nil
	}
	// Update the existing APIRule
	err = r.Client.Update(ctx, desiredAPIRule, &client.UpdateOptions{})
	return nil
}

func (r *SubscriptionReconciler) getRelevantSubscriptions(svcNs, svcName string, ctx context.Context) ([]eventingv1alpha1.Subscription, error) {
	subscriptions := &eventingv1alpha1.SubscriptionList{}
	relevantSubs := make([]eventingv1alpha1.Subscription, 0)
	err := r.Cache.List(ctx, subscriptions, &client.ListOptions{
		Namespace: svcNs,
	})
	if err != nil {
		return []eventingv1alpha1.Subscription{}, err
	}
	if len(subscriptions.Items) > 0 {
		for _, sub := range subscriptions.Items {
			// Filtering subscriptions which are being deleted at the moment
			if sub.DeletionGracePeriodSeconds != nil {
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
			if svcNs == svcNsForSub && svcName == svcNameForSub {
				relevantSubs = append(relevantSubs, sub)
			}
		}
	}
	return relevantSubs, nil
}

func (r *SubscriptionReconciler) makeAPIRule(svcNs, svcName string, labels map[string]string, subs []eventingv1alpha1.Subscription, sink url.URL) (*apigatewayv1alpha1.APIRule, error) {
	port, err := convertURLPortForApiRulePort(sink)
	if err != nil {
		return nil, errors.Wrap(err, "conversion from URL port to APIRule port failed")
	}

	randomSuffix := getRandSuffix(svcNs, svcName, SuffixLength)
	hostName := fmt.Sprintf("%s-%s", "webhook", randomSuffix)

	apiRule := object.NewAPIRule(svcNs, SinkURLPrefix,
		object.WithLabels(labels),
		object.WithOwnerReference(subs),
		object.WithService(hostName, svcName, port),
		object.WithGateway(ClusterLocalAPIGateway),
		object.WithRules(subs, http.MethodPost, http.MethodOptions))
	return apiRule, nil
}

func getRandSuffix(svcNs, svcName string, l int) string {
	svcNameNsHasher := fnv.New64()
	hashutil.DeepHashObject(svcNameNsHasher, fmt.Sprintf("%s-%s", svcNs, svcName))
	encodedStr := rand.SafeEncodeString(fmt.Sprint(svcNameNsHasher.Sum64()))
	encodedStrAsRune := []rune(encodedStr)
	return string(encodedStrAsRune[:l])
}

func convertURLPortForApiRulePort(sink url.URL) (uint32, error) {
	port := uint32(0)
	sinkPort := sink.Port()
	if sinkPort != "" {
		u64, err := strconv.ParseUint(sink.Port(), 10, 32)
		if err != nil {
			return port, errors.Wrapf(err, "failed to convert port: %s", sink.Port())
		}
		port = uint32(u64)
	}
	if port == uint32(0) {
		switch strings.ToLower(sink.Scheme) {
		case "http":
			port = uint32(80)
		case "https":
			port = uint32(443)
		}
	}
	return port, nil
}

// syncInitialStatus determines the desires initial status and updates it accordingly (if conditions changed)
func (r *SubscriptionReconciler) syncInitialStatus(subscription *eventingv1alpha1.Subscription, result *ctrl.Result, ctx context.Context) error {
	currentStatus := subscription.Status

	expectedStatus := eventingv1alpha1.SubscriptionStatus{}
	expectedStatus.InitializeConditions()

	// case: conditions are already initialized
	if len(currentStatus.Conditions) >= len(expectedStatus.Conditions) {
		return nil
	}

	subscription.Status = expectedStatus
	if err := r.Status().Update(ctx, subscription); err != nil {
		return err
	}
	result.Requeue = true

	return nil
}

// replaceStatusCondition replaces the given condition on the subscription
func (r *SubscriptionReconciler) replaceStatusCondition(subscription *eventingv1alpha1.Subscription, condition eventingv1alpha1.Condition) error {
	// compile list of desired conditions
	desiredConditions := make([]eventingv1alpha1.Condition, 0)
	for _, c := range subscription.Status.Conditions {
		if c.Type == condition.Type {
			// take given condition
			desiredConditions = append(desiredConditions, condition)
		} else {
			// take already present condition
			desiredConditions = append(desiredConditions, c)
		}
	}

	// prevent unnecessary updates
	if isEqualConditions(subscription.Status.Conditions, desiredConditions) {
		return nil
	}

	// update the status
	subscription.Status.Conditions = desiredConditions
	return nil
}

// emitConditionEvent emits a kubernetes event and sets the event type based on the Condition status
func (r *SubscriptionReconciler) emitConditionEvent(subscription *eventingv1alpha1.Subscription, condition eventingv1alpha1.Condition, reason string, message string) {
	eventType := corev1.EventTypeNormal
	if condition.Status == corev1.ConditionFalse {
		eventType = corev1.EventTypeWarning
	}
	r.recorder.Event(subscription, eventType, reason, message)
}

// TODO: do not update when nothing changed

func (r *SubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&eventingv1alpha1.Subscription{}).
		//For(&apigatewayv1alpha1.APIRule{}).
		Complete(r)
}

func (r *SubscriptionReconciler) addFinalizer(subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) error {
	subscription.ObjectMeta.Finalizers = append(subscription.ObjectMeta.Finalizers, FinalizerName)
	logger.V(1).Info("Adding finalizer")
	if err := r.Update(ctx, subscription); err != nil {
		return errors.Wrapf(err, "error while adding Finalizer with name: %s", FinalizerName)
	}
	logger.V(1).Info("Added finalizer")
	return nil
}

func (r *SubscriptionReconciler) removeFinalizer(subscription *eventingv1alpha1.Subscription, ctx context.Context, logger logr.Logger) error {
	var finalizers []string

	// Build finalizer list without the one the controller owns
	for _, finalizer := range subscription.ObjectMeta.Finalizers {
		if finalizer == FinalizerName {
			continue
		}
		finalizers = append(finalizers, finalizer)
	}

	logger.V(1).Info("Removing finalizer")
	subscription.ObjectMeta.Finalizers = finalizers
	if err := r.Update(ctx, subscription); err != nil {
		return errors.Wrapf(err, "error while removing Finalizer with name: %s", FinalizerName)
	}
	logger.V(1).Info("Removed finalizer")
	return nil
}

// isFinalizerSet checks if a finalizer is set on the Subscription which belongs to this controller
func (r *SubscriptionReconciler) isFinalizerSet(subscription *eventingv1alpha1.Subscription) bool {
	// Check if finalizer is already set
	for _, finalizer := range subscription.ObjectMeta.Finalizers {
		if finalizer == FinalizerName {
			return true
		}
	}
	return false
}

// isInDeletion checks if the Subscription shall be deleted
func (r *SubscriptionReconciler) isInDeletion(subscription *eventingv1alpha1.Subscription) bool {
	return !subscription.DeletionTimestamp.IsZero()
}

const timeoutRetryActiveEmsStatus = time.Second * 30

// checkStatusActive checks if the subscription is active and if not, sets a timer for retry
func (r *SubscriptionReconciler) checkStatusActive(subscription *eventingv1alpha1.Subscription) (statusChanged bool, retry bool, err error) {
	if subscription.Status.EmsSubscriptionStatus.SubscriptionStatus == string(types2.SubscriptionStatusActive) {
		if len(subscription.Status.FailedActivation) > 0 {
			subscription.Status.FailedActivation = ""
			statusChanged = true
		}
		return
	}
	t1 := time.Now()
	if len(subscription.Status.FailedActivation) == 0 {
		// it's the first time
		subscription.Status.FailedActivation = t1.Format(time.RFC3339)
		statusChanged = true
		retry = true
	} else {
		// check the timeout
		if t0, er := time.Parse(time.RFC3339, subscription.Status.FailedActivation); er != nil {
			err = er
		} else if t1.Sub(t0) > timeoutRetryActiveEmsStatus {
			err = fmt.Errorf("Timeout waiting for the subscription to be active: %v", subscription.Name)
		} else {
			retry = true
		}
	}
	return
}
