package libsystem

// I/O policy tiers (sys/resource.h), lowest priority last.
const (
	IOPolicyDefault  = 0
	IOPolicyStandard = 5
	IOPolicyUtility  = 4
	IOPolicyThrottle = 3
)
