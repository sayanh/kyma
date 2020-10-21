package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	types2 "github.com/kyma-project/kyma/components/eventing-controller/pkg/ems2/api/events/types"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/handlers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"time"

	// TODO: use different package
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
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
			return ctrl.Result{}, err
		}
		if result.Requeue {
			return result, nil
		}
		if err := r.syncInitialStatus(subscription, &result, ctx); err != nil {
			log.Error(err, "error while syncing status")
			return ctrl.Result{}, err
		}
		if result.Requeue {
			return result, nil
		}
	}

	// mark if the subscription status was changed
	var statusChanged bool

	// Sync with APIRule, expose the webhook
	if statusChangedForAPIRule, err := r.syncAPIRule(subscription, &result, ctx, log); err != nil {
		log.Error(err, "error while syncing API rule")
		return ctrl.Result{}, err
	} else {
		statusChanged = statusChanged || statusChangedForAPIRule
	}

	// Sync the BEB Subscription with the Subscription CR
	if statusChangedForBeb, err := r.syncBEBSubscription(subscription, &result, ctx, log); err != nil {
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

// syncBEBSubscription it returns
func (r *SubscriptionReconciler) syncBEBSubscription(subscription *eventingv1alpha1.Subscription,
	result *ctrl.Result, ctx context.Context, logger logr.Logger) (bool, error) {
	logger.Info("Syncing subscription with BEB")
	// TODO: get beb credentials from secret

	r.bebClient.Initialize()

	if r.isInDeletion(subscription) {
		logger.Info("Deleting BEB subscription")
		if err := r.bebClient.DeleteBebSubscription(subscription); err != nil {
			return false, err
		}
		return false, nil
	}

	var statusChanged bool
	var err error
	if statusChanged, err = r.bebClient.SyncBebSubscription(subscription); err != nil {
		logger.Error(err, "Update BEB subscription failed")
		return false, err
	}

	condition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, "Successfully synchronized with BEB subscription", corev1.ConditionTrue)
	if !subscription.Status.IsConditionSubscribed() {
		if err := r.replaceStatusCondition(subscription, condition); err != nil {
			return statusChanged, err
		}
		statusChanged = true
	}

	statusChangedAtCheck, retry, errTimeout := r.checkStatusActive(subscription)
	statusChanged = statusChanged || statusChangedAtCheck
	if errTimeout != nil {
		logger.Error(errTimeout, "Timeout at retry")
		result.Requeue = false
		return statusChanged, errTimeout
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
	return statusChanged, nil
}

func (r *SubscriptionReconciler) syncAPIRule(subscription *eventingv1alpha1.Subscription, result *ctrl.Result,
	ctx context.Context, logger logr.Logger) (bool, error) {
	// TODO
	var statusChanged bool

	return statusChanged, nil
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
func(r*SubscriptionReconciler) checkStatusActive (subscription *eventingv1alpha1.Subscription) (statusChanged bool, retry bool, err error) {
	if subscription.Status.EmsSubscriptionStatus.SubscriptionStatus == string(types2.SubscriptionStatusActive) {
		if len(subscription.Status.FailedActivation) >0  {
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
