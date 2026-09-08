package interceptor

import (
	"encoding/json"
	"fmt"
	"os"
)

// RoleMap is which uid holds which role. It is the whole of the store's
// authorization configuration: a file listing uids per role, and the uid the
// service itself runs as, which holds [RoleService] whether the file names
// it or not.
//
// There is no token scheme behind this and no principal table. A unix socket
// only ever reaches processes on this host, and SO_PEERCRED reports the
// account each of them runs as, so the account is the identity and a file
// mapping accounts to roles is the entire policy.
type RoleMap struct {
	// ServiceUID is the account the service runs as. Its calls are the
	// worker's own, so it holds every permission.
	ServiceUID int
	// Roles is every other uid the operator has named.
	Roles map[int]Role
}

// roleFile is the configuration on disk: a role, then the uids that hold it.
//
//	{"operator": [1001], "reader": [1500, 1501]}
type roleFile map[Role][]int

// DefaultRoleMap is the map a service with no configuration file uses: its
// own uid, and nothing else. Mill and the operator share an account at this
// maturity, so this is the ordinary case and not a degenerate one.
func DefaultRoleMap() RoleMap {
	return RoleMap{ServiceUID: os.Getuid(), Roles: map[int]Role{}}
}

// LoadRoleMap reads the map at path. An empty path returns
// [DefaultRoleMap]; a path that does not exist is an error, because an
// operator who named a file meant to use it.
func LoadRoleMap(path string) (RoleMap, error) {
	roles := DefaultRoleMap()
	if path == "" {
		return roles, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return RoleMap{}, fmt.Errorf("interceptor: read role map: %w", err)
	}
	var file roleFile
	if err := json.Unmarshal(content, &file); err != nil {
		return RoleMap{}, fmt.Errorf("interceptor: parse role map %s: %w", path, err)
	}
	for role, uids := range file {
		if _, ok := grants[role]; !ok {
			return RoleMap{}, fmt.Errorf("interceptor: role map %s: %q is not a role", path, role)
		}
		for _, uid := range uids {
			if uid < 0 {
				return RoleMap{}, fmt.Errorf("interceptor: role map %s: %d is not a uid", path, uid)
			}
			roles.Roles[uid] = role
		}
	}
	return roles, nil
}

// Role returns the role uid holds, and false when it holds none. The
// service's own uid always holds [RoleService]: it is the account the
// worker face of this binary runs as, and a file cannot demote it into
// being unable to write its own ledger.
func (m RoleMap) Role(uid int) (Role, bool) {
	if uid == m.ServiceUID {
		return RoleService, true
	}
	role, ok := m.Roles[uid]
	return role, ok
}
