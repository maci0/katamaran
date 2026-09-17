package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/maci0/katamaran/internal/orchestrator"
)

func TestReconciler_RecoverTimeoutOnStalledList(t *testing.T) {
	cr := newMigrationCR("m-stall", []string{finalizerName}, false, map[string]any{
		"phase":       "transferring",
		"migrationID": "id-stall",
	})
	rec, dyn, _ := newReconcilerWithCR(t, &fakeOrch{}, cr)
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rec.Kube = kube
	rec.StatusTimeout = 250 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	key := types.NamespacedName{Namespace: "default", Name: "m-stall"}
	rec.markTracking(key)
	go func() {
		defer close(done)
		rec.recover(ctx, key, cr)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never requested jobs")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled job list exceeded recovery timeout")
	}
	got, err := dyn.Resource(MigrationGVR).Namespace(key.Namespace).Get(context.Background(), key.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(got.Object, "status", "message")
	if phase != string(orchestrator.PhaseFailed) || message != "recovery timed out waiting for jobs" {
		t.Fatalf("status = %q, %q; want failed recovery timeout", phase, message)
	}
	if rec.isTracked(key) {
		t.Fatal("timed-out recovery remains tracked")
	}
}
