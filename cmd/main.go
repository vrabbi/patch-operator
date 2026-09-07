/*
Copyright 2026.

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
	"crypto/tls"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/controller"
	"github.com/vrabbi/patch-operator/internal/impersonate"
	"github.com/vrabbi/patch-operator/internal/targetcache"
	patchwebhook "github.com/vrabbi/patch-operator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(patchv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		secureMetrics        bool
		enableHTTP2          bool
		webhookPort          int
		webhookCertDir       string
		reauthorizeAfter     time.Duration
		disableWebhooks      bool
		maxWatchedGVKs       int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"Address the metrics endpoint binds to. :8443 for HTTPS, :8080 for HTTP, 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election. Required in HA: two writers on one target is exactly what this "+
			"operator exists to prevent.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"Enable HTTP/2 for the metrics and webhook servers.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server binds to.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory holding the webhook serving certificate.")
	flag.DurationVar(&reauthorizeAfter, "reauthorize-after", 10*time.Minute,
		"How often to re-run the SubjectAccessReview recorded at admission. Admission is "+
			"point-in-time, so this closes the revoked-RBAC gap. Zero disables the re-check.")
	flag.IntVar(&maxWatchedGVKs, "max-watched-target-kinds", targetcache.DefaultMaxGVKs,
		"Cap on distinct target kinds watched at once. Kinds beyond the cap converge on the "+
			"requeue interval rather than on events.")
	flag.BoolVar(&disableWebhooks, "disable-webhooks", false,
		"Do not register the admission webhooks. Local development only: it removes the "+
			"SubjectAccessReview boundary.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// HTTP/2 is off by default: it has a history of denial-of-service issues (CVE-2023-44487 and
	// CVE-2023-39325), and neither the metrics endpoint nor the webhook needs it.
	disableHTTP2 := func(c *tls.Config) {
		if !enableHTTP2 {
			c.NextProtos = []string{"http/1.1"}
		}
	}

	webhookServer := webhook.NewServer(webhook.Options{
		Port:    webhookPort,
		CertDir: webhookCertDir,
		TLSOpts: []func(*tls.Config){disableHTTP2},
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       []func(*tls.Config){disableHTTP2},
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "patch-operator.terasky.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	// The target-key index spans both contributor kinds, because after a promotion one tracker
	// holds a mix of them and has to list both by the same key.
	if err := controller.SetupIndexes(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to set up field indexes")
		os.Exit(1)
	}

	authorizer := &patchwebhook.Authorizer{
		Client: mgr.GetClient(),
		Mapper: mgr.GetRESTMapper(),
	}
	reauthorizer := &patchwebhook.Reauthorizer{Authorizer: authorizer}

	targets := targetcache.New(mgr, maxWatchedGVKs, nil)

	impersonation := impersonate.NewFactory(
		mgr.GetConfig(),
		client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()},
		mgr.GetClient(),
	)

	// Four controllers, two implementations. The scope split costs a validation table and a
	// tracker-selection rule, not a second copy of the reconcile logic.
	resourcePatchReconciler := &controller.ContributorReconciler[
		*patchv1alpha1.ResourcePatch, *patchv1alpha1.ResourcePatchList,
	]{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		APIReader:        mgr.GetAPIReader(),
		New:              func() *patchv1alpha1.ResourcePatch { return &patchv1alpha1.ResourcePatch{} },
		NewList:          func() *patchv1alpha1.ResourcePatchList { return &patchv1alpha1.ResourcePatchList{} },
		ReauthorizeAfter: reauthorizeAfter,
		Authorizer:       reauthorizer,
	}
	if err := resourcePatchReconciler.SetupWithManager(mgr, &patchv1alpha1.ResourcePatch{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourcePatch")
		os.Exit(1)
	}

	clusterResourcePatchReconciler := &controller.ContributorReconciler[
		*patchv1alpha1.ClusterResourcePatch, *patchv1alpha1.ClusterResourcePatchList,
	]{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		APIReader:        mgr.GetAPIReader(),
		New:              func() *patchv1alpha1.ClusterResourcePatch { return &patchv1alpha1.ClusterResourcePatch{} },
		NewList:          func() *patchv1alpha1.ClusterResourcePatchList { return &patchv1alpha1.ClusterResourcePatchList{} },
		ReauthorizeAfter: reauthorizeAfter,
		Authorizer:       reauthorizer,
	}
	if err := clusterResourcePatchReconciler.SetupWithManager(mgr, &patchv1alpha1.ClusterResourcePatch{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterResourcePatch")
		os.Exit(1)
	}

	sharedResourceReconciler := &controller.TrackerReconciler[
		*patchv1alpha1.SharedResource, *patchv1alpha1.SharedResourceList,
	]{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		New:           func() *patchv1alpha1.SharedResource { return &patchv1alpha1.SharedResource{} },
		Impersonation: impersonation,
		TargetCache:   targets,
	}
	if err := sharedResourceReconciler.SetupWithManager(mgr, &patchv1alpha1.SharedResource{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SharedResource")
		os.Exit(1)
	}

	clusterSharedResourceReconciler := &controller.TrackerReconciler[
		*patchv1alpha1.ClusterSharedResource, *patchv1alpha1.ClusterSharedResourceList,
	]{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		New:           func() *patchv1alpha1.ClusterSharedResource { return &patchv1alpha1.ClusterSharedResource{} },
		Impersonation: impersonation,
		TargetCache:   targets,
	}
	if err := clusterSharedResourceReconciler.SetupWithManager(mgr, &patchv1alpha1.ClusterSharedResource{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterSharedResource")
		os.Exit(1)
	}

	if !disableWebhooks {
		decoder := webhookadmission.NewDecoder(mgr.GetScheme())

		mgr.GetWebhookServer().Register("/validate-terasky-com-v1alpha1-resourcepatch",
			&webhook.Admission{Handler: &patchwebhook.ContributorValidator{
				New:        func() patchv1alpha1.Contributor { return &patchv1alpha1.ResourcePatch{} },
				Authorizer: authorizer,
				Decoder:    decoder,
			}})

		mgr.GetWebhookServer().Register("/validate-terasky-com-v1alpha1-clusterresourcepatch",
			&webhook.Admission{Handler: &patchwebhook.ContributorValidator{
				New:        func() patchv1alpha1.Contributor { return &patchv1alpha1.ClusterResourcePatch{} },
				Authorizer: authorizer,
				Decoder:    decoder,
			}})

		// The principal recorder is a mutating webhook, and has to be: a validating webhook's
		// patch is discarded by the API server. Mutating admission runs first, so the validator
		// still sees the final object.
		mgr.GetWebhookServer().Register("/mutate-terasky-com-v1alpha1-resourcepatch",
			&webhook.Admission{Handler: &patchwebhook.PrincipalRecorder{
				New:     func() patchv1alpha1.Contributor { return &patchv1alpha1.ResourcePatch{} },
				Decoder: decoder,
			}})

		mgr.GetWebhookServer().Register("/mutate-terasky-com-v1alpha1-clusterresourcepatch",
			&webhook.Admission{Handler: &patchwebhook.PrincipalRecorder{
				New:     func() patchv1alpha1.Contributor { return &patchv1alpha1.ClusterResourcePatch{} },
				Decoder: decoder,
			}})
	} else {
		setupLog.Info("admission webhooks are disabled; the SubjectAccessReview boundary is not in effect")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
