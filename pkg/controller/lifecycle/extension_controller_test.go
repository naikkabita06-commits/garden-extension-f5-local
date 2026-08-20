package lifecycle

import (
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestExtensionReconcilePredicate(t *testing.T) {
	p := extensionReconcilePredicate()
	base := &extensionsv1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{
			Generation:  1,
			Annotations: map[string]string{gardenerTimestampAnnotationKey: "2026-08-09T10:00:00Z"},
		},
	}

	tests := []struct {
		name string
		old  *extensionsv1alpha1.Extension
		new  *extensionsv1alpha1.Extension
		want bool
	}{
		{
			name: "status-only update is ignored",
			old:  base.DeepCopy(),
			new:  base.DeepCopy(),
			want: false,
		},
		{
			name: "new Gardener timestamp is accepted",
			old:  base.DeepCopy(),
			new: func() *extensionsv1alpha1.Extension {
				ex := base.DeepCopy()
				ex.Annotations[gardenerTimestampAnnotationKey] = "2026-08-09T10:01:00Z"
				return ex
			}(),
			want: true,
		},
		{
			name: "new operation request is accepted",
			old:  base.DeepCopy(),
			new: func() *extensionsv1alpha1.Extension {
				ex := base.DeepCopy()
				ex.Annotations[gardenerOperationAnnotationKey] = "reconcile"
				return ex
			}(),
			want: true,
		},
		{
			name: "controller clearing operation is ignored",
			old: func() *extensionsv1alpha1.Extension {
				ex := base.DeepCopy()
				ex.Annotations[gardenerOperationAnnotationKey] = "reconcile"
				return ex
			}(),
			new:  base.DeepCopy(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}); got != tt.want {
				t.Fatalf("Update() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestExtensionOutputIsCurrentAcknowledgesGardenerTimestamp(t *testing.T) {
	providerStatus := []byte(`{"controlPlaneVip":"10.0.0.1"}`)
	lastUpdate := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	ex := &extensionsv1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{
			Generation: 1,
			Annotations: map[string]string{
				gardenerTimestampAnnotationKey: "2026-08-09T10:01:00Z",
			},
		},
		Status: extensionsv1alpha1.ExtensionStatus{
			DefaultStatus: extensionsv1alpha1.DefaultStatus{
				ObservedGeneration: 1,
				ProviderStatus:      &runtime.RawExtension{Raw: providerStatus},
				LastOperation: &gardencorev1beta1.LastOperation{
					Type:           gardencorev1beta1.LastOperationTypeReconcile,
					State:          gardencorev1beta1.LastOperationStateSucceeded,
					Progress:       100,
					Description:    "done",
					LastUpdateTime: metav1.NewTime(lastUpdate),
				},
			},
		},
	}

	if extensionOutputIsCurrent(ex, providerStatus, gardencorev1beta1.LastOperationStateSucceeded, "done") {
		t.Fatal("status older than gardener.cloud/timestamp must be refreshed")
	}

	ex.Status.LastOperation.LastUpdateTime = metav1.NewTime(lastUpdate.Add(2 * time.Minute))
	if !extensionOutputIsCurrent(ex, providerStatus, gardencorev1beta1.LastOperationStateSucceeded, "done") {
		t.Fatal("status newer than gardener.cloud/timestamp should be current")
	}
}
