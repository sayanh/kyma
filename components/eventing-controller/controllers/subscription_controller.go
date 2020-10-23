package controllers

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	types2 "github.com/kyma-project/kyma/components/eventing-controller/pkg/ems2/api/events/types"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/handlers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	// TODO: use different package
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apigatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"
	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	oryv1alpha1 "github.com/ory/oathkeeper-maester/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SubscriptionReconciler reconciles a Subscription object
type SubscriptionReconciler struct {
	client.Client
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

const SinkURLPrefix = "webhook"

func NewSubscriptionReconciler(
	client client.Client,
	log logr.Logger,
	recorder record.EventRecorder,
	scheme *runtime.Scheme,
) *SubscriptionReconciler {
	bebClient := &handlers.Beb{
		Log: log,
	}
	return &SubscriptionReconciler{
		Client:    client,
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
	if err := r.Get(ctx, req.NamespacedName, cachedSubscription); err != nil {
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

	// Sync the BEB Subscription with the Subscription CR
	if err := r.syncBEBSubscription(subscription, &result, ctx, log); err != nil {
		log.Error(err, "error while syncing BEB subscription")
		return result, err
	}

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
	// Validate URL
	sURL, err := url.ParseRequestURI(subscription.Spec.Sink)
	if err != nil {
		logger.Error(err, "url is invalid")
		return nil
	}

	// Validate URL
	// DynamicInformer for svc
	parts := strings.Split(sURL.Host, ".")
	if len(parts) < 2 {
		// TODO use a proper log message
		logger.Error(fmt.Errorf("### Invalid Sink ###"), "### Invalid Sink ###")
		return nil
	}

	svcName := parts[0]
	svcNamespace := parts[1]
	svcLookupKey := types.NamespacedName{Name: svcName, Namespace: svcNamespace}
	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, svcLookupKey, svc); err != nil {
		// TODO use a proper log message
		logger.Error(err, "### Error while Get Sink ###")
		return nil
	}

	// Extract service and ns
	// Deletion

	//err = validateK8SURL(sURL)
	//if err != nil {
	//	logger.Error(err, "sink is not a valid k8s internal URL")
	//	return nil
	//}

	// Create or update APIRule
	err = r.createOrUpdateAPIRule(*sURL, result, ctx, logger)
	if err != nil {
		return errors.Wrap(err, "failed to createOrUpdateAPIRule")
	}
	return nil
}

func (r *SubscriptionReconciler) createOrUpdateAPIRule(sink url.URL, result *ctrl.Result, ctx context.Context, logger logr.Logger) error {
	// Look up APIRule
	// Diff
	// Update
	// or
	// Create
	sinkHostArr := strings.Split(sink.Host, ".")
	svcName := sinkHostArr[0]
	svcNs := sinkHostArr[1]
	isExternal := true
	gateway := "kyma-gateway.kyma-system.svc.cluster.local"
	handlerOAuth := "oauth2_introspection"
	port, err := convertURLPortForApiRulePort(sink)
	if err != nil {
		return errors.Wrap(err, "conversion from URL port to APIRule port failed")
	}

	labels := map[string]string{
		"service": svcName,
		"beb":     "webhook",
	}

	handler := oryv1alpha1.Handler{
		Name: handlerOAuth,
	}
	authenticator := &oryv1alpha1.Authenticator{
		Handler: &handler,
	}
	accessStrategies := []*oryv1alpha1.Authenticator{
		authenticator,
	}
	testHost := "test"
	apiRule := apigatewayv1alpha1.APIRule{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", SinkURLPrefix),
			Labels:       labels,
			Namespace:    svcNs,
		},

		Spec: apigatewayv1alpha1.APIRuleSpec{
			Service: &apigatewayv1alpha1.Service{
				Name:       &svcName,
				Port:       &port,
				Host:       &testHost,
				IsExternal: &isExternal,
			},
			Gateway: &gateway,
			Rules: []apigatewayv1alpha1.Rule{
				{
					Path: sink.Path,
					Methods: []string{
						http.MethodPost,
						http.MethodOptions,
					},
					AccessStrategies: accessStrategies,
				},
			},
		},
	}

	// TODO
	// Fetch existing ApiRule
	// If not existing then create
	err = r.Client.Create(ctx, &apiRule, &client.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to create ApiRule")
	}

	// If existing compute diff with desired
	// If there is diff update

	return nil
}

//func Create(obj interface{}, result interface{}) error {
//	u, err := ToUnstructured(obj)
//	if err != nil {
//		return err
//	}
//	var s dynamic.NamespaceableResourceInterface
//	var created *unstructured.Unstructured
//	if u.GetNamespace() == "" {
//		created, err = s.Client.Create(u, v1.CreateOptions{})
//	} else {
//		created, err = s.Client.Namespace(u.GetNamespace()).Create(u, v1.CreateOptions{})
//	}
//	if err != nil {
//		return err
//	}
//
//	return FromUnstructured(created, result)
//}
//
//func ToUnstructured(v interface{}) (*unstructured.Unstructured, error) {
//	if v == nil {
//		return nil, nil
//	}
//
//	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
//	if err != nil {
//		return nil, errors.Wrapf(err, "while converting resource %T to unstructured", v)
//	}
//	if len(u) == 0 {
//		return nil, nil
//	}
//
//	return &unstructured.Unstructured{Object: u}, nil
//}

//func FromUnstructured(obj *unstructured.Unstructured, v interface{}) error {
//	if obj == nil {
//		return nil
//	}
//
//	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, v)
//	if err != nil {
//		return errors.Wrapf(err, "while converting unstructured to resource %T %s", v, obj.Object)
//	}
//
//	return nil
//}

func convertURLPortForApiRulePort(sink url.URL) (uint32, error) {
	port := uint32(0)
	if sink.Port() != "" {
		u64, err := strconv.ParseUint(sink.Port(), 10, 32)
		if err != nil {
			return port, errors.Wrapf(err, "failed to convert port: %s", sink.Port())
		}
		port = uint32(u64)
	}
	if port != uint32(0) {
		switch strings.ToLower(sink.Scheme) {
		case "http":
			port = uint32(80)
		case "https":
			port = uint32(443)
		}
	}
	return port, nil
}

func validateK8SURL(sinkURL *url.URL) error {

	clusterLocalSuffix := "svc.cluster.local"
	host := strings.Split(sinkURL.Host, ":")[0]
	if !strings.HasSuffix(host, clusterLocalSuffix) {
		return fmt.Errorf("sink: %s is not a cluster internal URL", sinkURL.Host)
	}
	_, err := net.LookupHost(host)
	return err
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
