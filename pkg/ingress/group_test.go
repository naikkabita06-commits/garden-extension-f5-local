package ingress

import (
	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestResolveGroupSelectsSortedNamespaceMembers(t *testing.T) {
	a := networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b", Annotations: map[string]string{lbannotations.VIPGroup: "blue"}}}
	items := []networkingv1.Ingress{a, {ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a", Annotations: map[string]string{lbannotations.VIPGroup: "blue"}}}, {ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "x", Annotations: map[string]string{lbannotations.VIPGroup: "blue"}}}}
	members, id, err := ResolveGroup(&a, items)
	if err != nil {
		t.Fatal(err)
	}
	if id != "ingress-group/ns/blue" || len(members) != 2 || members[0].Name != "a" {
		t.Fatalf("unexpected group %q %#v", id, members)
	}
}
