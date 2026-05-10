package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/maci0/katamaran/internal/controller"
)

// generateWebhookCert returns a self-signed TLS cert + the PEM-encoded
// CA bundle suitable for the ValidatingWebhookConfiguration's caBundle
// field. SAN includes the full Service DNS form so the apiserver can
// dial https://<service>.<namespace>.svc:9443.
func generateWebhookCert(serviceName, namespace string) (tls.Certificate, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: serviceName + "." + namespace + ".svc"},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames: []string{
			serviceName + "." + namespace + ".svc",
			serviceName + "." + namespace + ".svc.cluster.local",
			serviceName,
		},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("X509KeyPair: %w", err)
	}
	return tlsCert, certPEM, nil
}

// patchWebhookConfigCABundle updates a ValidatingWebhookConfiguration's
// clientConfig.caBundle field to caBundle for every webhook entry.
// Idempotent; called once per leader-election win.
func patchWebhookConfigCABundle(ctx context.Context, kube kubernetes.Interface, configName string, caBundle []byte) error {
	cfg, err := kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, configName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.Info("ValidatingWebhookConfiguration not present; skipping caBundle patch", "name", configName)
			return nil
		}
		return fmt.Errorf("get %s: %w", configName, err)
	}
	changed := false
	for i := range cfg.Webhooks {
		if !bytesEqual(cfg.Webhooks[i].ClientConfig.CABundle, caBundle) {
			cfg.Webhooks[i].ClientConfig.CABundle = caBundle
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_, err = kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, cfg, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update %s: %w", configName, err)
	}
	slog.Info("ValidatingWebhookConfiguration caBundle patched", "name", configName)
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// webhookConfigName is the name of the ValidatingWebhookConfiguration
// shipped in config/crd/manager.yaml. Hard-coded because the patch is
// install-time, not user-configurable.
const webhookConfigName = "katamaran-mgr"

// serveWebhook starts the admission HTTPS server on addr. Blocks until
// ctx cancel. Cert is generated in-process; CA bundle is patched onto
// the ValidatingWebhookConfiguration so the apiserver trusts the cert.
func serveWebhook(ctx context.Context, addr, serviceName, namespace string, rec *controller.Reconciler, kube kubernetes.Interface) error {
	cert, caBundle, err := generateWebhookCert(serviceName, namespace)
	if err != nil {
		return fmt.Errorf("generate webhook cert: %w", err)
	}
	if err := patchWebhookConfigCABundle(ctx, kube, webhookConfigName, caBundle); err != nil {
		// Log + continue: the manifest may be applied later, or
		// failurePolicy=Ignore on the webhook keeps the cluster usable
		// even if the patch failed.
		slog.Warn("Failed to patch ValidatingWebhookConfiguration caBundle (webhook will still serve, but apiserver may not trust it)", "error", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admit", func(w http.ResponseWriter, r *http.Request) { handleAdmit(w, r, rec) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("Admission webhook listening", "addr", addr, "service", serviceName, "namespace", namespace)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("webhook server: %w", err)
	}
	return nil
}

// handleAdmit decodes the AdmissionReview, asks the reconciler whether
// the incoming Pod is a race-window replacement that must be denied,
// and writes the AdmissionReview response. failurePolicy=Ignore on the
// webhook config means decode/internal errors fall through to "allow",
// so the cluster never wedges on a busted webhook.
func handleAdmit(w http.ResponseWriter, r *http.Request, rec *controller.Reconciler) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 3<<20))
	if err != nil {
		writeAdmissionAllow(w, types.UID(""), fmt.Sprintf("read body: %v", err))
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		writeAdmissionAllow(w, types.UID(""), fmt.Sprintf("decode AdmissionReview: %v", err))
		return
	}
	uid := review.Request.UID
	resp := admissionv1.AdmissionResponse{UID: uid, Allowed: true}

	var pod corev1.Pod
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err != nil {
		resp.Result = &metav1.Status{Message: fmt.Sprintf("decode Pod: %v", err)}
		writeAdmissionResponse(w, review, resp)
		return
	}

	if msg := rec.ShouldDenyPodCreate(&pod); msg != "" {
		resp.Allowed = false
		resp.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  metav1.StatusReasonForbidden,
			Message: msg,
			Code:    http.StatusForbidden,
		}
		slog.Info("Admission webhook denied Pod create", "namespace", pod.Namespace, "generateName", pod.GenerateName, "reason", msg)
	}
	writeAdmissionResponse(w, review, resp)
}

func writeAdmissionResponse(w http.ResponseWriter, _ admissionv1.AdmissionReview, resp admissionv1.AdmissionResponse) {
	out := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: &resp,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		slog.Warn("Failed to encode admission response", "error", err)
	}
}

// writeAdmissionAllow short-circuits to an Allowed response when the
// webhook itself errored. Combined with failurePolicy=Ignore on the
// ValidatingWebhookConfiguration, this means a busted webhook never
// blocks pod creation cluster-wide.
func writeAdmissionAllow(w http.ResponseWriter, uid types.UID, msg string) {
	resp := admissionv1.AdmissionResponse{UID: uid, Allowed: true}
	if msg != "" {
		resp.Result = &metav1.Status{Message: msg}
	}
	out := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: &resp,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
