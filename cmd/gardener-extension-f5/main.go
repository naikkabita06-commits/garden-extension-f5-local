// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/logger"

	f5v1alpha1 "github.com/gardener/gardener-extension-f5/pkg/apis/f5/v1alpha1"
	"github.com/gardener/gardener-extension-f5/pkg/controller/lifecycle"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	webhookconversion "sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

func main() {
	runtimelog.SetLogger(logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON))

	ctx := signals.SetupSignalHandler()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	setupLog := ctrl.Log.WithName("setup")

	s := runtime.NewScheme()
	// Core K8s types (Service, Endpoints, Secret, ...)
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return fmt.Errorf("adding client-go scheme: %w", err)
	}
	// Gardener Extension API (extensions.gardener.cloud/v1alpha1 Extension)
	if err := extensionsv1alpha1.AddToScheme(s); err != nil {
		return fmt.Errorf("adding gardener extensions scheme: %w", err)
	}
	// Your CRD API (f5.extensions.gardener.cloud/v1alpha1)
	if err := f5v1alpha1.AddToScheme(s); err != nil {
		return fmt.Errorf("adding f5 scheme: %w", err)
	}

	cfg := ctrl.GetConfigOrDie()

	mgrOpts := ctrl.Options{
		Scheme:                 s,
		LeaderElection:         true,
		LeaderElectionID:       "gardener-extension-f5-leader",
		HealthProbeBindAddress: ":8081",
	}

	// Only enable the webhook server if TLS certs are present.
	// With a single API version (v1alpha1) the conversion webhook is a no-op,
	// so the extension can run without it.
	certDir := webhookCertDir()
	webhookEnabled := certExists(certDir)
	if webhookEnabled {
		setupLog.Info("TLS certs found, enabling webhook server", "certDir", certDir)
		mgrOpts.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    9443,
			CertDir: certDir,
		})
	} else {
		setupLog.Info("TLS certs not found, webhook server disabled", "certDir", certDir)
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	if err := lifecycle.AddToManager(mgr, ctrl.Log); err != nil {
		return fmt.Errorf("adding lifecycle controller: %w", err)
	}

	if webhookEnabled {
		mgr.GetWebhookServer().Register("/convert", webhookconversion.NewWebhookHandler(s))
		setupLog.Info("registered CRD conversion webhook", "path", "/convert")
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("starting manager: %w", err)
	}
	return nil
}

// webhookCertDir returns the TLS certificate directory for the webhook server.
// Override by setting WEBHOOK_CERT_DIR. Defaults to the controller-runtime standard path.
func webhookCertDir() string {
	if dir := os.Getenv("WEBHOOK_CERT_DIR"); dir != "" {
		return dir
	}
	return "/tmp/k8s-webhook-server/serving-certs"
}

// certExists returns true if tls.crt and tls.key are present in the given directory.
func certExists(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "tls.crt")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "tls.key")); err != nil {
		return false
	}
	return true
}
