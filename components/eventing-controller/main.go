package main

import (
	"flag"
	"github.com/kyma-project/kyma/components/eventing-controller/reconciler/apirule"
	"github.com/kyma-project/kyma/components/eventing-controller/reconciler/subscription"
	"log"
	"os"
	"time"

	"github.com/kyma-project/kyma/components/eventing-controller/pkg/env"

	apigatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = eventingv1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme

	_ = apigatewayv1alpha1.AddToScheme(scheme)
}

func main() {
	var metricsAddr string
	var resyncPeriod time.Duration
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.DurationVar(&resyncPeriod, "reconcile-period", time.Minute*10, "Period between triggering of reconciling calls")
	flag.Parse()
	// TODO: Add a flag to control this value
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	cfg := env.GetConfig()
	domain := cfg.Domain
	if len(domain) == 0 {
		log.Fatalf("env var DOMAIN should be a non-empty value")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		Port:               9443,
		SyncPeriod:         &resyncPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if err = subscription.NewReconciler(
		mgr.GetClient(),
		mgr.GetCache(),
		ctrl.Log.WithName("reconciler").WithName("Subscription"),
		mgr.GetEventRecorderFor("eventing-controller"),
		cfg,
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Subscription")
		os.Exit(1)
	}
	if err = apirule.NewReconciler(
		mgr.GetClient(),
		mgr.GetCache(),
		ctrl.Log.WithName("reconciler").WithName("APIRule"),
		mgr.GetEventRecorderFor("eventing-controller"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Subscription")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
