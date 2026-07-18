package finalizers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestEnsureAndRemoveFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	changed, err := Ensure(ctx, c, svc, "example.com/finalizer")
	if err != nil || !changed {
		t.Fatalf("Ensure changed=%t err=%v", changed, err)
	}
	stored := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(stored, "example.com/finalizer") {
		t.Fatalf("expected finalizer")
	}

	changed, err = Remove(ctx, c, stored, "example.com/finalizer")
	if err != nil || !changed {
		t.Fatalf("Remove changed=%t err=%v", changed, err)
	}
}
