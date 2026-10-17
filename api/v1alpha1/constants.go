package v1alpha1

const (
	// ControllerName is the controller name, field manager and managed-by label value.
	ControllerName = "hatchet-controller"

	// ManagedByLabel marks generated objects; the Secret cache only sees objects carrying it.
	ManagedByLabel = "app.kubernetes.io/managed-by"

	// TenantIDAnnotation binds a HatchetTenant to a Hatchet tenant (inherit and write-back).
	TenantIDAnnotation = "hatchet-controller.abigroup.io/tenant-id"

	// TenantFinalizer guards token revocation on HatchetTenant deletion.
	TenantFinalizer = "hatchet-controller.abigroup.io/finalizer"

	// WorkerLabel selects worker pods for the optional egress NetworkPolicy.
	WorkerLabel = "hatchet-controller.abigroup.io/worker"

	// Worker Secret annotations.
	TokenIDAnnotation   = "hatchet-controller.abigroup.io/token-id"
	IssuedAtAnnotation  = "hatchet-controller.abigroup.io/issued-at"
	ExpiresAtAnnotation = "hatchet-controller.abigroup.io/expires-at"
)

// Condition types.
const (
	ConditionAccepted = "Accepted"
	ConditionReady    = "Ready"
)

// Condition reasons.
const (
	ReasonAccepted         = "Accepted"
	ReasonInstanceNotFound = "InstanceNotFound"
	ReasonNotAllowed       = "NotAllowed"

	ReasonReady            = "Ready"
	ReasonConflict         = "Conflict"
	ReasonForbidden        = "Forbidden"
	ReasonUnauthorized     = "Unauthorized"
	ReasonInheritRejected  = "InheritRejected"
	ReasonTenantIDConflict = "TenantIDConflict"
	ReasonSlugTaken        = "SlugTaken"
	ReasonHatchetError     = "HatchetError"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonRevocationFailed = "RevocationFailed"
)
