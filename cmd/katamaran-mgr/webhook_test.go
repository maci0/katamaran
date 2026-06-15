package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/maci0/katamaran/internal/controller"
)

// postAdmit runs handleAdmit against an in-memory request and returns the
// decoded AdmissionReview response plus the HTTP status code.
func postAdmit(t *testing.T, body []byte) (admissionv1.AdmissionReview, int) {
	t.Helper()
	rec := controller.NewReconciler(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/admit", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	handleAdmit(rr, req, rec)

	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	return out, rr.Code
}

func admissionReviewWithPod(t *testing.T, uid string, pod corev1.Pod) []byte {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID(uid),
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	return body
}

// A well-formed request for a pod with no pending-adoption mark must be
// allowed, and the response must echo the request UID so the apiserver can
// correlate it.
func TestHandleAdmit_AllowsPodWithoutPendingMark(t *testing.T) {
	t.Parallel()
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "web-"}}
	out, code := postAdmit(t, admissionReviewWithPod(t, "uid-allow", pod))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Response == nil {
		t.Fatal("response is nil")
	}
	if !out.Response.Allowed {
		t.Fatalf("pod without pending mark must be allowed; result=%+v", out.Response.Result)
	}
	if out.Response.UID != types.UID("uid-allow") {
		t.Fatalf("response UID = %q, want uid-allow (must echo request UID)", out.Response.UID)
	}
}

// failurePolicy=Ignore relies on the handler failing OPEN: a body that is not
// a decodable AdmissionReview must still produce an Allowed response so a
// busted webhook never wedges pod creation cluster-wide.
func TestHandleAdmit_FailsOpenOnMalformedBody(t *testing.T) {
	t.Parallel()
	out, code := postAdmit(t, []byte("this is not json"))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("malformed body must fail open (Allowed=true); got %+v", out.Response)
	}
}

// Failing open is the dangerous path (race-window protection silently
// disabled), so it must be observable: webhookFailOpenTotal has to advance.
// Counters are process-global and other parallel tests may also fail open, so
// assert the delta is at least 1 rather than exactly 1.
func TestHandleAdmit_FailOpenIncrementsCounter(t *testing.T) {
	before := webhookFailOpenTotal.Value()
	postAdmit(t, []byte("this is not json"))
	if delta := webhookFailOpenTotal.Value() - before; delta < 1 {
		t.Fatalf("webhookFailOpenTotal delta = %d, want >= 1", delta)
	}
}

// An AdmissionReview that decodes but carries no Request must also fail open
// rather than panicking on the nil pointer.
func TestHandleAdmit_FailsOpenOnNilRequest(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(admissionv1.AdmissionReview{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, code := postAdmit(t, body)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("nil request must fail open (Allowed=true); got %+v", out.Response)
	}
}

// FuzzHandleAdmit drives the admission webhook's untrusted HTTP entry point
// with arbitrary request bodies. The webhook runs failurePolicy=Ignore, so the
// hard invariants are: it must never panic on malformed input (including a nil
// Request, a non-Pod embedded object, or truncated JSON), and it must always
// emit an HTTP 200 carrying a decodable AdmissionReview with a non-nil
// Response — anything else either crashes the webhook or wedges pod creation
// cluster-wide.
func FuzzHandleAdmit(f *testing.F) {
	rec := controller.NewReconciler(nil, nil, nil, nil)

	podRaw, err := json.Marshal(corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "web-"}})
	if err != nil {
		f.Fatalf("marshal seed pod: %v", err)
	}
	seedReview, err := json.Marshal(admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID("uid-seed"),
			Object: runtime.RawExtension{Raw: podRaw},
		},
	})
	if err != nil {
		f.Fatalf("marshal seed review: %v", err)
	}
	seeds := [][]byte{
		[]byte(""),
		[]byte("not json"),
		[]byte("{}"),
		[]byte("[1,2,3]"),
		[]byte(`{"request":null}`),
		[]byte(`{"request":{"uid":"u","object":{"raw":"!!!"}}}`),
		seedReview,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest(http.MethodPost, "/admit", bytes.NewReader(body))
		rr := httptest.NewRecorder()
		handleAdmit(rr, req, rec) // must not panic

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (webhook must always answer)", rr.Code)
		}
		var out admissionv1.AdmissionReview
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not a decodable AdmissionReview: %v (body=%q)", err, rr.Body.String())
		}
		if out.Response == nil {
			t.Fatalf("response has nil Response field; body=%q", rr.Body.String())
		}
	})
}

// A request whose embedded object is not a decodable Pod must fail open while
// still echoing the request UID.
func TestHandleAdmit_FailsOpenOnUndecodablePod(t *testing.T) {
	t.Parallel()
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID("uid-baspod"),
			Object: runtime.RawExtension{Raw: []byte("[1,2,3]")}, // valid JSON, but not a Pod object
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, code := postAdmit(t, body)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Response == nil || !out.Response.Allowed {
		t.Fatalf("undecodable pod must fail open (Allowed=true); got %+v", out.Response)
	}
	if out.Response.UID != types.UID("uid-baspod") {
		t.Fatalf("response UID = %q, want uid-baspod", out.Response.UID)
	}
}
