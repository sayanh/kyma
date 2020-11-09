package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	// gcp auth etc.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	apigatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"

	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/constants"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/ems/api/events/config"
	bebtypes "github.com/kyma-project/kyma/components/eventing-controller/pkg/ems/api/events/types"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/env"
	"github.com/kyma-project/kyma/components/eventing-controller/reconciler/apirule"
	reconcilertesting "github.com/kyma-project/kyma/components/eventing-controller/testing"
)

const (
	subscriptionNamespacePrefix = "test-"
	subscriptionID              = "test-subs-1"
	bigPollingInterval          = 3 * time.Second
	bigTimeOut                  = 40 * time.Second
	smallTimeOut                = 5 * time.Second
	smallPollingInterval        = 1 * time.Second
)

var _ = Describe("Subscription Reconciliation Tests", func() {
	var namespaceName string

	// enable me for debugging
	// SetDefaultEventuallyTimeout(time.Minute)
	// SetDefaultEventuallyPollingInterval(time.Second)

	BeforeEach(func() {
		namespaceName = getUniqueNamespaceName()
		// we need to reset the http requests which the mock captured
		beb.Reset()
	})

	AfterEach(func() {
		// detailed request logs
		logf.Log.V(1).Info("beb requests", "number", len(beb.Requests))

		i := 0
		for req, payloadObject := range beb.Requests {
			reqDescription := fmt.Sprintf("method: %q, url: %q, payload object: %+v", req.Method, req.RequestURI, payloadObject)
			fmt.Printf("request[%d]: %s\n", i, reqDescription)
			i++
		}

		// print all subscriptions in the namespace for debugging purposes
		if err := printSubscriptions(namespaceName); err != nil {
			logf.Log.Error(err, "error while printing subscriptions")
		}
	})

	When("Creating a valid Subscription with webhook followed by a creation of APIRule", func() {
		It("Should create a valid APIRule", func() {
			ctx := context.Background()
			subscriptionName := "sub-create-api-rule"

			// Ensuring subscriber svc
			subscriberSvc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(subscriberSvc, ctx)

			// Create subscription
			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithFilter, reconcilertesting.WithWebhook)
			reconcilertesting.WithValidSink(namespaceName, subscriberSvc.Name, givenSubscription)
			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Creating a valid APIRule")
			getAPIRuleForASvc(subscriberSvc, ctx).Should(reconcilertesting.HaveValidAPIRule(*givenSubscription))

			By("Updating the APIRule(replicating apigateway controller) status to be Ready")
			apiRuleCreated := filterAPIRulesForASvc(getAPIRules(ctx, subscriberSvc), subscriberSvc)
			ensureAPIRuleStatusUpdatedWithStatusReady(&apiRuleCreated, ctx).Should(BeNil())

			By("Creating a BEB Subscription")
			var bebSubscription bebtypes.Subscription
			Eventually(func() bool {
				for r, payloadObject := range beb.Requests {
					if reconcilertesting.IsBebSubscriptionCreate(r, *beb.BebConfig) {
						bebSubscription = payloadObject.(bebtypes.Subscription)
						receivedSubscriptionName := bebSubscription.Name
						// ensure the correct subscription was created
						return subscriptionName == receivedSubscriptionName
					}
				}
				return false
			}).Should(BeTrue())

			By("Setting a subscription active condition")
			subscriptionActiveCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscriptionActive, eventingv1alpha1.ConditionReasonSubscriptionActive, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionActiveCondition),
			))

			By("Setting APIRule status in Subscription to Ready")
			subscriptionAPIReadyCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionAPIRuleStatus, eventingv1alpha1.ConditionReasonAPIRuleStatusReady, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionAPIReadyCondition),
			))

			By("Marking the subscription as ready")
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.HaveSubscriptionReady())
		})
	})

	When("Creating a Subscription with invalid Sink", func() {
		It("Should stop reconciling", func() {
			ctx := context.Background()
			subscriptionName := "sub-create-with-invalid-sink"
			// Ensuring subscriber svc
			subscriberSvc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(subscriberSvc, ctx)

			// Create subscription
			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithFilter, reconcilertesting.WithWebhook)
			givenSubscription.Spec.Sink = "invalid"
			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Setting APIRule status to False")
			subscriptionAPIReadyFalseCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionAPIRuleStatus, eventingv1alpha1.ConditionReasonAPIRuleStatusNotReady, v1.ConditionFalse)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionAPIReadyFalseCondition),
			))

			By("Deleting the object to not provoke more reconciliation requests")
			Expect(k8sClient.Delete(ctx, givenSubscription)).Should(BeNil())
			getSubscription(givenSubscription, ctx).ShouldNot(reconcilertesting.HaveSubscriptionFinalizer(Finalizer))

			//getK8sEvents(&subscriptionEvents, givenSubscription.Namespace).Should(HaveEvent(subscriptionDeletedEvent))
		})
	})
	PWhen("Creating a valid Subscription without webhook with already existing APIRule", func() {})
	PWhen("Creating a valid Subscription with invalid APIRule", func() {})
	PWhen("Subscription changed with creation of APIRule", func() {})
	PWhen("Subscription changed in sink path with update of APIRule", func() {
		It("Should append to the array of rules in APIRule", func() {})
	})
	PWhen("Subscription changed in sink path with update of APIRule", func() {
		It("Should shrink the array of rules in APIRule", func() {})
	})
	PWhen("Subscription changed in sink port with update of APIRule", func() {
		It("Should add the Subscription to the ownerreferences of APIRule and remove it from the old APIRule ", func() {})
	})
	PWhen("Creating a valid Subscription(with webhook) with already existing APIRule", func() {
		It("Should reconcile the Subscription", func() {
			subscriptionName := "test-valid-subscription-1"
			ctx := context.Background()

			// Ensuring subscriber svc
			subscriberSvc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(subscriberSvc, ctx)

			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithFilter, reconcilertesting.WithWebhook)
			reconcilertesting.WithValidSink(namespaceName, subscriberSvc.Name, givenSubscription)

			// Ensuring existing APIRule
			apiRule := reconcilertesting.NewAPIRuleWithoutOwnRef("foo", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(subscriberSvc.Name, subscriberSvc.Name, apiRule)
			apiRule.Namespace = namespaceName
			apiRule.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  subscriberSvc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRule, givenSubscription.Spec.Sink)

			ensureAPIRuleCreated(apiRule, ctx)

			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Setting a finalizer")
			//var subscription = &eventingv1alpha1.Subscription{}
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveSubscriptionFinalizer(Finalizer),
			))

			By("Setting a subscribed condition")
			subscriptionCreatedCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, eventingv1alpha1.ConditionReasonSubscriptionCreated, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionCreatedCondition),
			))

			By("Emitting a subscription created event")
			var subscriptionEvents = v1.EventList{}
			subscriptionCreatedEvent := v1.Event{
				Reason:  string(eventingv1alpha1.ConditionReasonSubscriptionCreated),
				Message: "",
				Type:    v1.EventTypeNormal,
			}
			getK8sEvents(&subscriptionEvents, givenSubscription.Namespace).Should(reconcilertesting.HaveEvent(subscriptionCreatedEvent))

			By("Setting a subscription active condition")
			subscriptionActiveCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscriptionActive, eventingv1alpha1.ConditionReasonSubscriptionActive, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionActiveCondition),
			))

			By("Emitting a subscription active event")
			subscriptionActiveEvent := v1.Event{
				Reason:  string(eventingv1alpha1.ConditionReasonSubscriptionActive),
				Message: "",
				Type:    v1.EventTypeNormal,
			}
			getK8sEvents(&subscriptionEvents, givenSubscription.Namespace).Should(reconcilertesting.HaveEvent(subscriptionActiveEvent))

			By("Creating a BEB Subscription")
			var bebSubscription bebtypes.Subscription
			Eventually(func() bool {
				for r, payloadObject := range beb.Requests {
					if reconcilertesting.IsBebSubscriptionCreate(r, *beb.BebConfig) {
						bebSubscription = payloadObject.(bebtypes.Subscription)
						receivedSubscriptionName := bebSubscription.Name
						// ensure the correct subscription was created
						return subscriptionName == receivedSubscriptionName
					}
				}
				return false
			}).Should(BeTrue())

			By("Updating APIRule")
			getAPIRule(apiRule, ctx).Should(reconcilertesting.HaveValidAPIRule(*givenSubscription))

			By("Marking it as ready")
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.HaveSubscriptionReady())

		})
	})

	PWhen("Subscription changed with already existing APIRule", func() {
		It("Should update the BEB subscription", func() {
			subscriptionName := "test-subscription-sub-changed"

			ctx := context.Background()
			// Create a subscription
			oldSvc := reconcilertesting.NewSubscriberSvc("webhook-old", namespaceName)
			ensureSubscriberSvcCreated(oldSvc, ctx)

			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithFilter, reconcilertesting.WithWebhook)
			reconcilertesting.WithValidSink(oldSvc.Namespace, oldSvc.Name, givenSubscription)

			apiRuleForOldSvc := reconcilertesting.NewAPIRuleWithoutOwnRef("api-old", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(oldSvc.Name, oldSvc.Name, apiRuleForOldSvc)
			apiRuleForOldSvc.Namespace = namespaceName
			apiRuleForOldSvc.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  oldSvc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRuleForOldSvc, givenSubscription.Spec.Sink)
			ensureAPIRuleCreated(apiRuleForOldSvc, ctx)
			getAPIRule(apiRuleForOldSvc, ctx).Should(reconcilertesting.HaveApiRuleReady())

			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Given subscription is ready")
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.HaveSubscriptionReady())

			// Update a subscription
			newSvc := reconcilertesting.NewSubscriberSvc("webhook-new", namespaceName)
			ensureSubscriberSvcCreated(newSvc, ctx)

			apiRuleForNewSvc := reconcilertesting.NewAPIRuleWithoutOwnRef("api-new", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(newSvc.Name, newSvc.Name, apiRuleForNewSvc)
			apiRuleForNewSvc.Namespace = namespaceName
			apiRuleForNewSvc.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  newSvc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			newSink := fmt.Sprintf("http://%s.%s.svc.cluster.local", newSvc.Name, newSvc.Namespace)
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRuleForNewSvc, newSink)

			ensureAPIRuleCreated(apiRuleForNewSvc, ctx)

			By("Updating the sink")
			givenSubscription.Spec.Sink = newSink
			updateSubscriptionSink(givenSubscription, ctx).Should(BeNil())
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.HaveSink(newSink))
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.HaveSubscriptionReady())

			By("Updating the BEB Subscription with the new sink")
			bebCreationRequests := make([]bebtypes.Subscription, 0)
			getBebSubscriptionCreationRequests(bebCreationRequests).Should(And(
				ContainElement(MatchFields(IgnoreMissing|IgnoreExtras,
					Fields{
						"Name":       BeEquivalentTo(givenSubscription.Name),
						"WebhookUrl": ContainSubstring("https://webhook-new"),
					},
				))))
			By("Updating APIRule")
			getAPIRule(apiRuleForNewSvc, ctx).Should(reconcilertesting.HaveValidAPIRule(*givenSubscription))
		})
	})

	PWhen("BEB subscription creation failed with existing APIRule", func() {
		It("Should not mark the subscription as ready", func() {
			subscriptionName := "test-subscription-beb-not-status-not-ready"
			ctx := context.Background()

			// Ensuring subscriber svc
			subscriberSvc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(subscriberSvc, ctx)

			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithWebhook, reconcilertesting.WithFilter)
			reconcilertesting.WithValidSink(subscriberSvc.Namespace, subscriberSvc.Name, givenSubscription)

			// Ensuring existing APIRule
			apiRule := reconcilertesting.NewAPIRuleWithoutOwnRef("foo", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(subscriberSvc.Name, subscriberSvc.Name, apiRule)
			apiRule.Namespace = namespaceName
			apiRule.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  subscriberSvc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRule, givenSubscription.Spec.Sink)
			ensureAPIRuleCreated(apiRule, ctx)

			By("preparing mock to simulate creation of BEB subscription failing on BEB side")
			beb.CreateResponse = func(w http.ResponseWriter) {
				// ups ... server returns 500
				w.WriteHeader(http.StatusInternalServerError)
				s := bebtypes.Response{
					StatusCode: http.StatusInternalServerError,
					Message:    "sorry, but this mock does not let you create a BEB subscription",
				}
				err := json.NewEncoder(w).Encode(s)
				Expect(err).ShouldNot(HaveOccurred())
			}

			// Create subscription
			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Setting APIRule status to Ready")
			subscriptionAPIReadyCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionAPIRuleStatus, eventingv1alpha1.ConditionReasonAPIRuleStatusReady, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionAPIReadyCondition),
			))

			By("Setting a subscription not created condition")
			subscriptionNotCreatedCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscribed, eventingv1alpha1.ConditionReasonSubscriptionCreationFailed, v1.ConditionFalse)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionNotCreatedCondition),
			))

			By("Marking subscription as not ready")
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				Not(reconcilertesting.HaveSubscriptionReady()),
			))

			By("Deleting the object to not provoke more reconciliation requests")
			Expect(k8sClient.Delete(ctx, givenSubscription)).Should(BeNil())
			getSubscription(givenSubscription, ctx).ShouldNot(reconcilertesting.HaveSubscriptionFinalizer(Finalizer))

			By("Updating APIRule")
			getAPIRule(apiRule, ctx).Should(reconcilertesting.HaveValidAPIRule(*givenSubscription))
		})
	})

	PWhen("BEB subscription status is not ready with existing APIRule", func() {
		It("Should not mark the subscription as ready", func() {
			subscriptionName := "test-subscription-beb-not-status-not-ready-2"
			ctx := context.Background()
			// Ensuring subscriber svc
			svc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(svc, ctx)

			givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithWebhook, reconcilertesting.WithFilter)
			reconcilertesting.WithValidSink(svc.Namespace, svc.Name, givenSubscription)

			// Ensuring existing APIRule
			apiRule := reconcilertesting.NewAPIRuleWithoutOwnRef("foo", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(svc.Name, svc.Name, apiRule)
			apiRule.Namespace = namespaceName
			apiRule.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  svc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRule, givenSubscription.Spec.Sink)
			ensureAPIRuleCreated(apiRule, ctx)

			isBebSubscriptionCreated := false

			By("preparing mock to simulate a non ready BEB subscription")
			beb.GetResponse = func(w http.ResponseWriter, subscriptionName string) {
				// until the BEB subscription creation call was performed, send successful get requests
				if !isBebSubscriptionCreated {
					reconcilertesting.BebGetSuccess(w, subscriptionName)
				} else {
					// after the BEB subscription was created, set the status to paused
					w.WriteHeader(http.StatusOK)
					s := bebtypes.Subscription{
						Name: subscriptionName,
						// ups ... BEB Subscription status is now paused
						SubscriptionStatus: bebtypes.SubscriptionStatusPaused,
					}
					err := json.NewEncoder(w).Encode(s)
					Expect(err).ShouldNot(HaveOccurred())
				}
			}
			beb.CreateResponse = func(w http.ResponseWriter) {
				isBebSubscriptionCreated = true
				reconcilertesting.BebCreateSuccess(w)
			}

			// Create subscription
			ensureSubscriptionCreated(givenSubscription, ctx)

			By("Setting APIRule status to Ready")
			subscriptionAPIReadyCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionAPIRuleStatus, eventingv1alpha1.ConditionReasonAPIRuleStatusReady, v1.ConditionTrue)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionAPIReadyCondition),
			))

			By("Setting a subscription not active condition")
			subscriptionNotActiveCondition := eventingv1alpha1.MakeCondition(eventingv1alpha1.ConditionSubscriptionActive, eventingv1alpha1.ConditionReasonSubscriptionNotActive, v1.ConditionFalse)
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				reconcilertesting.HaveCondition(subscriptionNotActiveCondition),
			))

			By("Marking it as not ready")
			getSubscription(givenSubscription, ctx).Should(And(
				reconcilertesting.HaveSubscriptionName(subscriptionName),
				Not(reconcilertesting.HaveSubscriptionReady()),
			))

			By("Deleting the object to not provoke more reconciliation requests")
			Expect(k8sClient.Delete(ctx, givenSubscription)).Should(BeNil())
			getSubscription(givenSubscription, ctx).ShouldNot(reconcilertesting.HaveSubscriptionFinalizer(Finalizer))
		})
	})

	PWhen("Deleting a valid Subscription", func() {
		It("Should reconcile the Subscription", func() {
			subscriptionName := "test-delete-valid-subscription-1"
			ctx := context.Background()
			givenSubscription := reconcilertesting.FixtureValidSubscription(subscriptionName, namespaceName, subscriptionID)
			processedBebRequests := 0
			svc := reconcilertesting.NewSubscriberSvc("webhook", namespaceName)
			ensureSubscriberSvcCreated(svc, ctx)

			// Ensuring existing APIRule
			apiRule := reconcilertesting.NewAPIRuleWithoutOwnRef("foo", reconcilertesting.WithoutPath, reconcilertesting.WithGateway, reconcilertesting.WithStatusReady)
			reconcilertesting.WithService(svc.Name, svc.Name, apiRule)
			apiRule.Namespace = namespaceName
			apiRule.Labels = map[string]string{
				constants.ControllerServiceLabelKey:  svc.Name,
				constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
			}
			reconcilertesting.SetSinkSvcPortInAPIRule(apiRule, givenSubscription.Spec.Sink)
			ensureAPIRuleCreated(apiRule, ctx)

			// Create subscription
			ensureSubscriptionCreated(givenSubscription, ctx)
			getAPIRule(apiRule, ctx).Should(reconcilertesting.HaveValidAPIRule(*givenSubscription))

			Context("Given the subscription is ready", func() {
				getSubscription(givenSubscription, ctx).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveSubscriptionReady(),
				))

				By("Creating a BEB Subscription")
				var bebSubscription bebtypes.Subscription
				Eventually(func() bool {
					for r, payloadObject := range beb.Requests {
						if reconcilertesting.IsBebSubscriptionCreate(r, *beb.BebConfig) {
							bebSubscription = payloadObject.(bebtypes.Subscription)
							receivedSubscriptionName := bebSubscription.Name
							// ensure the correct subscription was created
							return subscriptionName == receivedSubscriptionName
						}
						processedBebRequests++
					}
					return false
				}).Should(BeTrue())
			})

			By("Deleting the Subscription")
			Expect(k8sClient.Delete(ctx, givenSubscription)).Should(BeNil())

			By("Removing the Subscription")
			//getSubscription(givenSubscription, ctx).ShouldNot(HaveSubscriptionFinalizer(Finalizer))
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.IsAnEmptySubscription())

			By("Deleting the BEB Subscription")
			Eventually(func() bool {
				i := -1
				for r := range beb.Requests {
					i++
					// only consider requests against beb after the subscription creation request
					if i <= processedBebRequests {
						continue
					}
					if reconcilertesting.IsBebSubscriptionDelete(r) {
						receivedSubscriptionName := reconcilertesting.GetRestAPIObject(r.URL)
						// ensure the correct subscription was created
						return subscriptionName == receivedSubscriptionName
					}
				}
				return false
			}).Should(BeTrue())

			By("Emitting some k8s events")
			var subscriptionEvents = v1.EventList{}
			subscriptionDeletedEvent := v1.Event{
				Reason:  string(eventingv1alpha1.ConditionReasonSubscriptionDeleted),
				Message: "",
				Type:    v1.EventTypeWarning,
			}

			By("Removing the APIRule")
			getSubscription(givenSubscription, ctx).Should(reconcilertesting.IsAnEmptySubscription())
			// TODO: Check for disappearing of APIRule which does not work in kubebuilder suite
			//getAPIRule(apiRule, ctx).Should(IsAnEmptyAPIRule())

			getK8sEvents(&subscriptionEvents, givenSubscription.Namespace).Should(reconcilertesting.HaveEvent(subscriptionDeletedEvent))
		})
	})

	DescribeTable("Schema tests: ensuring required fields are not treated as optional",
		func(subscription *eventingv1alpha1.Subscription) {
			ctx := context.Background()
			subscription.Namespace = namespaceName

			By("Letting the APIServer reject the custom resource")
			ensureSubscriptionCreationFails(subscription, ctx)
		},
		Entry("filter missing",
			func() *eventingv1alpha1.Subscription {
				subscription := reconcilertesting.FixtureValidSubscription("schema-filter-missing", "", subscriptionID)
				subscription.Spec.Filter = nil
				return subscription
			}()),
		Entry("protocolsettings missing",
			func() *eventingv1alpha1.Subscription {
				subscription := reconcilertesting.FixtureValidSubscription("schema-filter-missing", "", subscriptionID)
				subscription.Spec.ProtocolSettings = nil
				return subscription
			}()),
	)

	DescribeTable("Schema tests: ensuring optional fields are not treated as required",
		func(subscription *eventingv1alpha1.Subscription) {
			ctx := context.Background()
			namespaceName := getUniqueNamespaceName()
			subscription.Namespace = namespaceName

			By("Letting the APIServer reject the custom resource")
			ensureSubscriptionCreated(subscription, ctx)
		},
		Entry("protocolsettings.webhookauth missing",
			func() *eventingv1alpha1.Subscription {
				subscription := reconcilertesting.FixtureValidSubscription("schema-filter-missing", "", subscriptionID)
				subscription.Spec.ProtocolSettings.WebhookAuth = nil
				return subscription
			}()),
	)
})

// getSubscription fetches a subscription using the lookupKey and allows to make assertions on it
func getSubscription(subscription *eventingv1alpha1.Subscription, ctx context.Context) AsyncAssertion {
	return Eventually(func() eventingv1alpha1.Subscription {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		if err := k8sClient.Get(ctx, lookupKey, subscription); err != nil {
			log.Printf("failed to fetch subscription(%s): %v", lookupKey.String(), err)
			return eventingv1alpha1.Subscription{}
		}
		log.Printf("like what: %v", subscription.Status)
		return *subscription
	}, bigTimeOut, bigPollingInterval)
}

func updateSubscriptionSink(subscription *eventingv1alpha1.Subscription, ctx context.Context) AsyncAssertion {
	return Eventually(func() error {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		existingSub := &eventingv1alpha1.Subscription{}
		if err := k8sClient.Get(ctx, lookupKey, existingSub); err != nil {
			return err
		}
		subToBeUpdated := existingSub.DeepCopy()
		subToBeUpdated.Spec.Sink = subscription.Spec.Sink

		if err := k8sClient.Update(ctx, subToBeUpdated); err != nil {
			return err
		}
		return nil
	}, time.Second*10, time.Second)
}

// getK8sEvents returns all kubernetes events for the given namespace.
// The result can be used in a gomega assertion.
func getK8sEvents(eventList *v1.EventList, namespace string) AsyncAssertion {
	ctx := context.TODO()
	return Eventually(func() v1.EventList {
		err := k8sClient.List(ctx, eventList, client.InNamespace(namespace))
		if err != nil {
			return v1.EventList{}
		}
		return *eventList
	})
}

// ensureAPIRuleCreated creates an APIRule with status READY
func ensureAPIRuleCreated(apiRule *apigatewayv1alpha1.APIRule, ctx context.Context) {
	By(fmt.Sprintf("Ensuring the test namespace %q is created", apiRule.Namespace))
	if apiRule.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(apiRule.Namespace)
		if namespace.Name != "default" {
			err := k8sClient.Create(ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				fmt.Println(err)
				Expect(err).ShouldNot(HaveOccurred())
			}
		}
	}

	By(fmt.Sprintf("Ensuring the APIRule %q is created", apiRule.Name))
	// create subscription
	err := k8sClient.Create(ctx, apiRule)
	if !k8serrors.IsAlreadyExists(err) {
		fmt.Println(err)
		Expect(err).Should(BeNil())
	}
	// update status only once when APIRule creation succeeds
	if err == nil {
		err = k8sClient.Status().Update(ctx, apiRule)
		Expect(err).Should(BeNil())
	}
}

// ensureAPIRuleStatusUpdated updates the status fof the APIRule(mocking APIGateway controller)
func ensureAPIRuleStatusUpdatedWithStatusReady(apiRule *apigatewayv1alpha1.APIRule, ctx context.Context) AsyncAssertion {
	By(fmt.Sprintf("Ensuring the APIRule %q is updated", apiRule.Name))

	return Eventually(func() error {

		lookupKey := types.NamespacedName{
			Namespace: apiRule.Namespace,
			Name:      apiRule.Name,
		}
		err := k8sClient.Get(ctx, lookupKey, apiRule)
		if err != nil {
			return err
		}
		newAPIRule := apiRule.DeepCopy()
		reconcilertesting.WithStatusReady(newAPIRule)
		err = k8sClient.Status().Update(ctx, newAPIRule)
		if err != nil {
			return err
		}
		log.Printf("apirule is updated: %v", apiRule)
		return nil
	}, bigTimeOut, bigPollingInterval)
}

// ensureSubscriptionCreated creates a Subscription in the k8s cluster. If a custom namespace is used, it will be created as well.
func ensureSubscriptionCreated(subscription *eventingv1alpha1.Subscription, ctx context.Context) {

	By(fmt.Sprintf("Ensuring the test namespace %q is created", subscription.Namespace))
	if subscription.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(subscription.Namespace)
		if namespace.Name != "default" {
			err := k8sClient.Create(ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				fmt.Println(err)
				Expect(err).ShouldNot(HaveOccurred())
			}
		}
	}

	By(fmt.Sprintf("Ensuring the subscription %q is created", subscription.Name))
	// create subscription
	err := k8sClient.Create(ctx, subscription)
	Expect(err).Should(BeNil())
}

// ensureSubscriberSvcCreated creates a Service in the k8s cluster. If a custom namespace is used, it will be created as well.
func ensureSubscriberSvcCreated(svc *corev1.Service, ctx context.Context) {

	By(fmt.Sprintf("Ensuring the test namespace %q is created", svc.Namespace))
	if svc.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(svc.Namespace)
		if namespace.Name != "default" {
			err := k8sClient.Create(ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				fmt.Println(err)
				Expect(err).ShouldNot(HaveOccurred())
			}
		}
	}

	By(fmt.Sprintf("Ensuring the subscriber service %q is created", svc.Name))
	// create subscription
	err := k8sClient.Create(ctx, svc)
	Expect(err).Should(BeNil())
}

// getBebSubscriptionCreationRequests filters the http requests made against BEB and returns the BEB Subscriptions
func getBebSubscriptionCreationRequests(bebSubscriptions []bebtypes.Subscription) AsyncAssertion {

	return Eventually(func() []bebtypes.Subscription {

		for r, payloadObject := range beb.Requests {
			if reconcilertesting.IsBebSubscriptionCreate(r, *beb.BebConfig) {
				bebSubscription := payloadObject.(bebtypes.Subscription)
				bebSubscriptions = append(bebSubscriptions, bebSubscription)
			}
		}
		return bebSubscriptions
	}, bigTimeOut, bigPollingInterval)
}

// ensureSubscriptionCreationFails creates a Subscription in the k8s cluster and ensures that it is reject because of invalid schema
func ensureSubscriptionCreationFails(subscription *eventingv1alpha1.Subscription, ctx context.Context) {
	if subscription.Namespace != "default " {
		namespace := fixtureNamespace(subscription.Namespace)
		if namespace.Name != "default" {
			Expect(k8sClient.Create(ctx, namespace)).Should(BeNil())
		}
	}
	Expect(k8sClient.Create(ctx, subscription)).Should(
		And(
			// prevent nil-pointer stacktrace
			Not(BeNil()),
			reconcilertesting.IsK8sUnprocessableEntity(),
		),
	)
}

func fixtureNamespace(name string) *v1.Namespace {
	namespace := v1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Namespace",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return &namespace
}

// printSubscriptions prints all subscriptions in the given namespace
func printSubscriptions(namespace string) error {
	// print subscription details
	ctx := context.TODO()
	subscriptionList := eventingv1alpha1.SubscriptionList{}
	if err := k8sClient.List(ctx, &subscriptionList, client.InNamespace(namespace)); err != nil {
		logf.Log.V(1).Info("error while getting subscription list", "error", err)
		return err
	}
	subscriptions := make([]string, 0)
	for _, sub := range subscriptionList.Items {
		subscriptions = append(subscriptions, sub.Name)
	}
	log.Printf("subscriptions: %v", subscriptions)
	return nil
}

func generateTestSuiteID() int {
	var seededRand = rand.New(
		rand.NewSource(time.Now().UnixNano()))
	return seededRand.Int()
}

func getUniqueNamespaceName() string {
	testSuiteID := generateTestSuiteID()
	namespaceName := fmt.Sprintf("%s%d", subscriptionNamespacePrefix, testSuiteID)
	return namespaceName
}

func getAPIRule(apiRule *apigatewayv1alpha1.APIRule, ctx context.Context) AsyncAssertion {
	return Eventually(func() apigatewayv1alpha1.APIRule {
		lookUpKey := types.NamespacedName{
			Namespace: apiRule.Namespace,
			Name:      apiRule.Name,
		}
		if err := k8sClient.Get(ctx, lookUpKey, apiRule); err != nil {
			log.Printf("failed to fetch APIRule(%s): %v", lookUpKey.String(), err)
			return apigatewayv1alpha1.APIRule{}
		}
		apiRuleList := &apigatewayv1alpha1.APIRuleList{}
		err := k8sClient.List(ctx, apiRuleList, &client.ListOptions{
			Namespace: apiRule.Namespace,
		})
		if err != nil {
			log.Printf("failed to list APIRules: %v", err)
		}
		log.Printf("apiRules :%d", len(apiRuleList.Items))
		if len(apiRuleList.Items) > 0 && len(apiRule.OwnerReferences) > 0 {
			log.Printf("#### owner ref ####")
			log.Printf("oR: %v", apiRule.OwnerReferences[0])
			sub := &eventingv1alpha1.Subscription{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Namespace: apiRule.Namespace,
				Name:      apiRule.OwnerReferences[0].Name,
			}, sub)
			if err != nil {
				log.Printf("failed to get sub: %v", err)
			} else {
				log.Printf("sub is %s", sub.Name)
			}
		}

		return *apiRule
	}, bigTimeOut, bigPollingInterval)
}

func filterAPIRulesForASvc(apiRules *apigatewayv1alpha1.APIRuleList, svc *corev1.Service) apigatewayv1alpha1.APIRule {
	log.Printf("apirules got ::: %v", apiRules)
	if len(apiRules.Items) == 1 && *apiRules.Items[0].Spec.Service.Name == svc.Name {
		return apiRules.Items[0]
	}
	return apigatewayv1alpha1.APIRule{}
}

func getAPIRules(ctx context.Context, svc *corev1.Service) *apigatewayv1alpha1.APIRuleList {
	labels := map[string]string{
		constants.ControllerServiceLabelKey:  svc.Name,
		constants.ControllerIdentityLabelKey: constants.ControllerIdentityLabelValue,
	}
	apiRules := &apigatewayv1alpha1.APIRuleList{}
	err := k8sClient.List(ctx, apiRules, &client.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(labels),
		Namespace:     svc.Namespace,
	})
	Expect(err).Should(BeNil())
	return apiRules
}

func getAPIRuleForASvc(svc *corev1.Service, ctx context.Context) AsyncAssertion {
	return Eventually(func() apigatewayv1alpha1.APIRule {
		apiRules := getAPIRules(ctx, svc)
		log.Printf("apirules got ::: %v", apiRules)
		return filterAPIRulesForASvc(apiRules, svc)
	}, smallTimeOut, smallPollingInterval)
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Test Suite setup ////////////////////////////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// These tests use Ginkgo (BDD-style Go controllertesting framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

// TODO: make configurable
const (
	useExistingCluster       = false
	attachControlPlaneOutput = false
)

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var beb *reconcilertesting.BebMock

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"Controller Suite",
		[]Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func(done Done) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(GinkgoWriter)))

	By("bootstrapping test environment")
	useExistingCluster := useExistingCluster
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../", "config", "crd", "bases"),
			filepath.Join("../../", "config", "crd", "external"),
		},
		AttachControlPlaneOutput: attachControlPlaneOutput,
		UseExistingCluster:       &useExistingCluster,
	}

	var err error

	cfg, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = eventingv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = apigatewayv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	// +kubebuilder:scaffold:scheme

	bebMock := startBebMock()
	//client, err := client.New()
	// Source: https://book.kubebuilder.io/cronjob-tutorial/writing-tests.html
	syncPeriod := time.Second
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme.Scheme,
		SyncPeriod: &syncPeriod,
	})
	Expect(err).ToNot(HaveOccurred())
	envConf := &env.Config{
		BebApiUrl:                bebMock.MessagingURL,
		ClientID:                 "foo-id",
		ClientSecret:             "foo-secret",
		TokenEndpoint:            bebMock.TokenURL,
		WebhookActivationTimeout: 0,
		WebhookClientID:          "foo-client-id",
		WebhookClientSecret:      "foo-client-secret",
		WebhookTokenEndpoint:     "foo-token-endpoint",
		WebhookAuthType:          "oauth",
		WebhookGrantType:         "client_credentials",
		Domain:                   "domain.com",
	}
	err = NewReconciler(
		k8sManager.GetClient(),
		k8sManager.GetCache(),
		ctrl.Log.WithName("reconciler").WithName("Subscription"),
		k8sManager.GetEventRecorderFor("eventing-controller"),
		envConf,
	).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	err = apirule.NewReconciler(
		k8sManager.GetClient(),
		k8sManager.GetCache(),
		ctrl.Log.WithName("reconciler").WithName("APIRule"),
		k8sManager.GetEventRecorderFor("eventing-controller"),
	).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctrl.SetupSignalHandler())
		Expect(err).ToNot(HaveOccurred())
	}()

	k8sClient = k8sManager.GetClient()
	Expect(k8sClient).ToNot(BeNil())

	close(done)
}, 60)

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

// startBebMock starts the beb mock and configures the controller process to use it
func startBebMock() *reconcilertesting.BebMock {
	By("Preparing BEB Mock")
	bebConfig := &config.Config{}
	beb = reconcilertesting.NewBebMock(bebConfig)
	bebURI := beb.Start()
	logf.Log.Info("beb mock listening at", "address", bebURI)
	tokenURL := fmt.Sprintf("%s%s", bebURI, reconcilertesting.TokenURLPath)
	messagingURL := fmt.Sprintf("%s%s", bebURI, reconcilertesting.MessagingURLPath)
	beb.TokenURL = tokenURL
	beb.MessagingURL = messagingURL
	bebConfig = config.GetDefaultConfig(messagingURL)
	beb.BebConfig = bebConfig
	return beb
}
