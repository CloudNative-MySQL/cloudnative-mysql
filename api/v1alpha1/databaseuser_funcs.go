/*
Copyright 2026 The CNMSQL - CloudNative for MySQL Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// deniedDynamicPrivileges are the cluster-control privileges a DatabaseUser must
// never be granted: they would let a tenant break replication, fencing, server
// configuration, or the operator's own accounts. This is the grant-level
// equivalent of the spec.mysql.parameters denylist and is the real safety net
// behind the "safe DBaaS superuser" recipe (ALL without WITH GRANT OPTION).
var deniedDynamicPrivileges = map[string]bool{
	"replication_slave_admin":     true,
	"replication_applier":         true,
	"group_replication_admin":     true,
	"group_replication_stream":    true,
	"system_variables_admin":      true,
	"connection_admin":            true,
	"service_connection_admin":    true,
	"persist_ro_variables_admin":  true,
	"binlog_admin":                true,
	"binlog_encryption_admin":     true,
	"clone_admin":                 true,
	"super":                       true,
	"shutdown":                    true,
	"file":                        true,
	"group_replication_flow_control_admin": true,
}

// UserName returns the resolved MySQL user name, defaulting to the resource name.
func (u *DatabaseUser) UserName() string {
	if u.Spec.Name != "" {
		return u.Spec.Name
	}
	return u.Name
}

// AdoptRequested reports whether the adopt annotation opts this user into taking
// ownership of a pre-existing MySQL account.
func (u *DatabaseUser) AdoptRequested() bool {
	return strings.EqualFold(u.Annotations[DatabaseUserAdoptAnnotation], "true")
}

// SetDefaults fills in the implicit defaults the API server would otherwise
// apply, so reconciliation and validation see a fully-populated spec.
func (u *DatabaseUser) SetDefaults() {
	if u.Spec.Host == "" {
		u.Spec.Host = "%"
	}
	if u.Spec.Ensure == "" {
		u.Spec.Ensure = EnsurePresent
	}
	if u.Spec.RequireTLS == "" {
		u.Spec.RequireTLS = "none"
	}
	if u.Spec.ReclaimPolicy == "" {
		u.Spec.ReclaimPolicy = "retain"
	}
	for i := range u.Spec.Grants {
		if u.Spec.Grants[i].On == "" {
			u.Spec.Grants[i].On = "*.*"
		}
	}
}

// Validate checks a DatabaseUser: the name must not be reserved, the host must
// be set, superuser and explicit grants are mutually exclusive, RequireTLS must
// be valid, and no grant may request a denied cluster-control privilege.
func (u *DatabaseUser) Validate() field.ErrorList {
	var allErrs field.ErrorList
	spec := field.NewPath("spec")

	name := u.UserName()
	if name == "" {
		allErrs = append(allErrs, field.Required(spec.Child("name"), "user name is required"))
	} else if isReservedRoleName(name) {
		allErrs = append(allErrs, field.Invalid(spec.Child("name"), name,
			"user name is reserved (root, mysql.*, cnmsql_*)"))
	}
	if u.Spec.Host == "" {
		allErrs = append(allErrs, field.Required(spec.Child("host"), "user host is required"))
	}
	if u.Spec.Superuser && len(u.Spec.Grants) > 0 {
		allErrs = append(allErrs, field.Invalid(spec.Child("grants"), u.Spec.Grants,
			"grants cannot be set when superuser is true"))
	}
	switch u.Spec.RequireTLS {
	case "", "none", "ssl", "x509":
	default:
		allErrs = append(allErrs, field.Invalid(spec.Child("requireTLS"), u.Spec.RequireTLS,
			"requireTLS must be one of none, ssl, x509"))
	}
	for i := range u.Spec.Grants {
		for _, priv := range u.Spec.Grants[i].Privileges {
			if deniedDynamicPrivileges[strings.ToLower(strings.TrimSpace(priv))] {
				allErrs = append(allErrs, field.Invalid(
					spec.Child("grants").Index(i).Child("privileges"), priv,
					"privilege is denied: it would let the user break replication, fencing, or operator accounts"))
			}
		}
	}
	return allErrs
}
