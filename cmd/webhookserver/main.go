/*
Copyright 2021 The actions-runner-controller authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/kelseyhightower/envconfig"
	actionsv1alpha1 "github.com/summerwind/actions-runner-controller/api/v1alpha1"
	"github.com/summerwind/actions-runner-controller/controllers"
	"github.com/summerwind/actions-runner-controller/github"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/exec"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = actionsv1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		err error

		metricsAddr          string
		enableLeaderElection bool
		syncPeriod           time.Duration
	)

	var c github.Config
	err = envconfig.Process("github", &c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: Environment variable read failed.")
	}

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&c.Token, "github-webhook-secret-token", c.Token, "The secret token of the GitHub Webhook. See https://docs.github.com/en/developers/webhooks-and-events/securing-your-webhooks")
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Minute, "Determines the minimum frequency at which K8s resources managed by this controller are reconciled. When you use autoscaling, set to a lower value like 10 minute, because this corresponds to the minimum time to react on demand change")
	flag.Parse()

	logger := zap.New(func(o *zap.Options) {
		o.Development = true
	})

	ctrl.SetLogger(logger)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		LeaderElection:     enableLeaderElection,
		Port:               9443,
		SyncPeriod:         &syncPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	hraWebhook := &controllers.HorizontalRunnerAutoscalerWebhook{
		Client:         mgr.GetClient(),
		Log:            ctrl.Log.WithName("controllers").WithName("Runner"),
		Recorder:       nil,
		Scheme:         mgr.GetScheme(),
		SecretKeyBytes: nil,
		WatchNamespace: "",
	}

	if err = hraWebhook.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Runner")
		os.Exit(1)
	}

	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())

	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()

		setupLog.Info("starting webhook server")
		if err := mgr.Start(ctx.Done()); err != nil {
			setupLog.Error(err, "problem running manager")
			os.Exit(1)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", hraWebhook.Handle)

	srv := http.Server{
		Addr:    ":3000",
		Handler: mux,
	}

	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()

		go func() {
			<-ctx.Done()

			srv.Shutdown(context.Background())
		}()

		if err := srv.ListenAndServe(); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				setupLog.Error(err, "problem running http server")
			}
		}
	}()

	go func() {
		<-ctrl.SetupSignalHandler()
		cancel()
	}()

	wg.Wait()
}
