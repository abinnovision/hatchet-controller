package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/abinnovision/hatchet-controller/api/v1alpha1"
)

const guardMessage = "annotation cannot be changed or removed once the tenant is bound"

var (
	guardOnce sync.Once
	guardErr  string
)

// installGuard applies the tenant-id-guard component and waits until the policy is enforced.
func installGuard(t *testing.T) {
	t.Helper()
	requireEnv(t)
	dir := filepath.Join("..", "..", "config", "components", "tenant-id-guard")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("tenant-id-guard component not found: %v", err)
	}
	guardOnce.Do(func() {
		ctx := context.Background()
		var policy admissionregistrationv1.ValidatingAdmissionPolicy
		var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
		for file, obj := range map[string]client.Object{"policy.yaml": &policy, "binding.yaml": &binding} {
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err == nil {
				err = yaml.UnmarshalStrict(b, obj)
			}
			if err == nil {
				err = k8s.Create(ctx, obj)
			}
			if err != nil {
				guardErr = file + ": " + err.Error()
				return
			}
		}

		// Policies load asynchronously; probe until a change on a bound tenant is rejected.
		probe := boundTenant(t, "probe-id")
		deadline := time.Now().Add(30 * time.Second)
		for i := 0; time.Now().Before(deadline); i++ {
			probe.Annotations[v1alpha1.TenantIDAnnotation] = "changed-" + strconv.Itoa(i)
			if err := k8s.Update(ctx, probe); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		guardErr = "policy not enforced within 30s"
	})
	if guardErr != "" {
		t.Fatal(guardErr)
	}
}

// boundTenant creates a tenant with the annotation (if non-empty) and status.tenantID set.
func boundTenant(t *testing.T, annotation string) *v1alpha1.HatchetTenant {
	t.Helper()
	tenant := newTenant(uniqueName("guard"))
	if annotation != "" {
		tenant.Annotations = map[string]string{v1alpha1.TenantIDAnnotation: annotation}
	}
	create(t, tenant)
	tenant.Status.TenantID = "5f0e6c1b-0000-4000-8000-000000000001"
	if err := k8s.Status().Update(context.Background(), tenant); err != nil {
		t.Fatalf("status update: %v", err)
	}
	return tenant
}

func TestGuardRejectsAnnotationChangeOnBoundTenant(t *testing.T) {
	installGuard(t)
	tenant := boundTenant(t, "5f0e6c1b-0000-4000-8000-000000000001")
	tenant.Annotations[v1alpha1.TenantIDAnnotation] = "other"
	requireRejected(t, k8s.Update(context.Background(), tenant), guardMessage)
}

func TestGuardRejectsAnnotationRemovalOnBoundTenant(t *testing.T) {
	installGuard(t)
	tenant := boundTenant(t, "5f0e6c1b-0000-4000-8000-000000000001")
	delete(tenant.Annotations, v1alpha1.TenantIDAnnotation)
	requireRejected(t, k8s.Update(context.Background(), tenant), guardMessage)
}

func TestGuardAllowsFirstSetOfAnnotationOnBoundTenant(t *testing.T) {
	installGuard(t)
	tenant := boundTenant(t, "")
	tenant.Annotations = map[string]string{v1alpha1.TenantIDAnnotation: tenant.Status.TenantID}
	if err := k8s.Update(context.Background(), tenant); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestGuardAllowsAnnotationChangeWhileUnbound(t *testing.T) {
	installGuard(t)
	tenant := newTenant(uniqueName("guard"))
	tenant.Annotations = map[string]string{v1alpha1.TenantIDAnnotation: "wrong"}
	create(t, tenant)
	tenant.Annotations[v1alpha1.TenantIDAnnotation] = "fixed"
	if err := k8s.Update(context.Background(), tenant); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestGuardAllowsUnrelatedUpdateOnBoundTenant(t *testing.T) {
	installGuard(t)
	tenant := boundTenant(t, "5f0e6c1b-0000-4000-8000-000000000001")
	tenant.Spec.DisplayName = "renamed"
	if err := k8s.Update(context.Background(), tenant); err != nil {
		t.Fatalf("update: %v", err)
	}
}
