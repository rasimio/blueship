package core

// RoleToolQuerier resolves the ordered tool allowlist for a role.
// *RoleToolStore is the in-memory implementation.
type RoleToolQuerier interface {
	Get(role string) []string
}

// RoleToolStore maps a role name (e.g. "cortex", "reflex", "background")
// to the ordered list of tool names that role is allowed to use.
//
// Source-of-truth lives in code: agents pass a Config.RoleTools map at
// boot, which the Ship wraps via NewRoleToolStore. Roles with no entries
// default to "all tools available" — handlers that need a stricter
// allowlist should declare an explicit list.
type RoleToolStore struct {
	roles map[string][]string
}

// NewRoleToolStore wraps an in-memory map. The map is not copied; the
// caller must not mutate it after construction.
func NewRoleToolStore(roles map[string][]string) *RoleToolStore {
	if roles == nil {
		roles = map[string][]string{}
	}
	return &RoleToolStore{roles: roles}
}

// Get returns the ordered tool names for a role, or nil when the role
// has no entry (meaning "no allowlist — handler decides").
func (s *RoleToolStore) Get(role string) []string {
	if s == nil {
		return nil
	}
	return s.roles[role]
}

// RoleToolKey is the map key for a role under a prompt profile. A profiled
// entry is optional: an empty profile, or a profile with no entry of its own,
// resolves to the plain role key, so introducing profiles cannot change an
// existing deployment.
//
// The separator is "@" rather than "/" or ":" because these keys are only ever
// map lookups — never filesystem paths, never provider:model strings — and a
// character that cannot be mistaken for either makes a misuse obvious on sight.
func RoleToolKey(role, profile string) string {
	if profile == "" {
		return role
	}
	return role + "@" + profile
}
