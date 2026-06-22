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

package cmd

import (
	"reflect"
	"testing"

	mysqlv1alpha1 "github.com/cnmsql/cnmsql/api/v1alpha1"
)

func TestParseGrantFlags(t *testing.T) {
	got, err := parseGrantFlags([]string{"SELECT,INSERT ON app.*", "ALL"})
	if err != nil {
		t.Fatal(err)
	}
	want := []mysqlv1alpha1.DatabaseUserGrant{
		{Privileges: []string{"SELECT", "INSERT"}, On: "app.*"},
		{Privileges: []string{"ALL"}, On: "*.*"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGrantFlags = %+v, want %+v", got, want)
	}
}

func TestParseGrantFlagsEmptyPrivileges(t *testing.T) {
	if _, err := parseGrantFlags([]string{" ON app.*"}); err == nil {
		t.Errorf("expected error for grant with no privileges")
	}
}

func TestApplyGrantDelta(t *testing.T) {
	existing := []mysqlv1alpha1.DatabaseUserGrant{
		{Privileges: []string{"SELECT"}, On: "app.*"},
		{Privileges: []string{"SELECT"}, On: "logs.*"},
	}
	add := []mysqlv1alpha1.DatabaseUserGrant{{Privileges: []string{"SELECT", "INSERT"}, On: "app.*"}}
	remove := []mysqlv1alpha1.DatabaseUserGrant{{On: "logs.*"}}

	got := applyGrantDelta(existing, add, remove)
	want := []mysqlv1alpha1.DatabaseUserGrant{
		{Privileges: []string{"SELECT", "INSERT"}, On: "app.*"}, // replaced
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyGrantDelta = %+v, want %+v", got, want)
	}
}

func TestSecretRefOrDefault(t *testing.T) {
	cases := []struct {
		ref, resource, wantName, wantKey string
	}{
		{"", "tenant", "tenant-pw", "password"},
		{"my-secret/pw", "tenant", "my-secret", "pw"},
		{"my-secret", "tenant", "my-secret", "password"},
	}
	for _, c := range cases {
		name, key := secretRefOrDefault(c.ref, c.resource)
		if name != c.wantName || key != c.wantKey {
			t.Errorf("secretRefOrDefault(%q,%q) = (%q,%q), want (%q,%q)", c.ref, c.resource, name, key, c.wantName, c.wantKey)
		}
	}
}
