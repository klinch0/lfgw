package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/VictoriaMetrics/metricsql"
	"gopkg.in/yaml.v3"
)

// ACLMap stores a parsed YAML with role defitions
type ACLMap map[string]ACL

// rolesToRawACL returns a comma-separated list of ACL definitions for all specified roles. Basically, it lets you dynamically generate a raw ACL as if it was supplied through acl.yaml. To support Assumed Roles, unknown roles are treated as ACL definitions.
func (a ACLMap) rolesToRawACL(roles []string) (string, error) {
	rawACLs := make([]string, 0, len(roles))

	for _, role := range roles {
		acl, exists := a[role]
		if exists {
			// NOTE: You should never see an empty definitions in .RawACL as those should be removed by toSlice further down the process. The error check below is not necessary, is left as an additional safeguard for now and might get removed in the future.
			if acl.RawACL == "" {
				return "", fmt.Errorf("%s role contains empty rawACL", role)
			}
			rawACLs = append(rawACLs, acl.RawACL)
		} else {
			// NOTE: Role names are not linted, so they may contain regular expressions, including the admin definition: .*
			rawACLs = append(rawACLs, role)
		}
	}

	rawACL := strings.Join(rawACLs, ", ")
	if rawACL == "" {
		return "", fmt.Errorf("constructed empty rawACL")
	}

	return rawACL, nil
}

// getACL takes a list of roles found in an OIDC claim and constructs and ACL based on them. If assumed roles are disabled, then only known roles (present in app.ACLMap) are considered.
func (a ACLMap) getACL(oidcRoles []string, assumedRolesEnabled bool) (ACL, error) {
	roles := []string{}
	assumedRoles := []string{}

	for _, role := range oidcRoles {
		_, exists := a[role]
		if exists {
			if a[role].Fullaccess {
				return a[role], nil
			}
			roles = append(roles, role)
		} else {
			assumedRoles = append(assumedRoles, role)
		}
	}

	if assumedRolesEnabled {
		roles = append(roles, assumedRoles...)
	}

	if len(roles) == 0 {
		return ACL{}, fmt.Errorf("no matching roles found")
	}

	// We can return a prebuilt ACL if there's only one role and it's known
	if len(roles) == 1 {
		role := roles[0]
		acl, exists := a[role]
		if exists {
			return acl, nil
		}
	}

	// To simplify creation of composite ACLs, we need to form a raw ACL, so the further process would be equal to what we have for processing acl.yaml
	rawACL, err := a.rolesToRawACL(roles)
	if err != nil {
		return ACL{}, err
	}

	acl := ACL{}

	acl, err = acl.PrepareACL(rawACL)
	if err != nil {
		return ACL{}, err
	}

	return acl, nil
}

// ACL stores a role definition
type ACL struct {
	Fullaccess  bool
	LabelFilter metricsql.LabelFilter
	RawACL      string
}

// toSlice converts namespace rules to string slices.
func (a *ACL) toSlice(str string) ([]string, error) {
	buffer := []string{}
	// yaml values should come already with trimmed leading and trailing spaces
	for _, s := range strings.Split(str, ",") {
		// in case there are empty elements in between
		s := strings.TrimSpace(s)

		for _, ch := range s {
			if unicode.IsSpace(ch) {
				return nil, fmt.Errorf("line should not contain spaces within individual elements (%q)", str)
			}
		}

		if s != "" {
			buffer = append(buffer, s)
		}
	}

	if len(buffer) == 0 {
		return nil, fmt.Errorf("line has to contain at least one valid element (%q)", str)
	}

	return buffer, nil
}

// PrepareACL returns an ACL based on a rule definition (non-regexp for one namespace, regexp - for many). .RawACL in the resulting value will contain a normalized value (anchors stripped, implicit admin will have only .*).
func (a *ACL) PrepareACL(rawACL string) (ACL, error) {
	var lf = metricsql.LabelFilter{
		Label:      "namespace",
		IsNegative: false,
		IsRegexp:   false,
	}

	buffer, err := a.toSlice(rawACL)
	if err != nil {
		return ACL{}, err
	}

	// If .* is in the slice, then we can omit any other value
	for _, v := range buffer {
		// TODO: move to a helper?
		if v == ".*" {
			// Note: with this approach, we intentionally omit other values in the resulting ACL
			return a.getFullaccessACL(), nil
		}
	}

	if len(buffer) == 1 {
		// TODO: move to a helper?
		if strings.ContainsAny(buffer[0], `.+*?^$()[]{}|\`) {
			lf.IsRegexp = true
			// Trim anchors as they're not needed for Prometheus, and not expected in the app.shouldBeModified function
			buffer[0] = strings.TrimLeft(buffer[0], "^")
			buffer[0] = strings.TrimLeft(buffer[0], "(")
			buffer[0] = strings.TrimRight(buffer[0], "$")
			buffer[0] = strings.TrimRight(buffer[0], ")")
		}
		lf.Value = buffer[0]
	} else {
		// "Regex matches are fully anchored. A match of env=~"foo" is treated as env=~"^foo$"." https://prometheus.io/docs/prometheus/latest/querying/basics/
		lf.Value = strings.Join(buffer, "|")
		lf.IsRegexp = true
	}

	if lf.IsRegexp {
		_, err := regexp.Compile(lf.Value)
		if err != nil {
			return ACL{}, fmt.Errorf("%s in %q (converted from %q)", err, lf.Value, rawACL)
		}
	}

	acl := ACL{
		Fullaccess:  false,
		LabelFilter: lf,
		RawACL:      strings.Join(buffer, ", "),
	}

	return acl, nil
}

// getFullaccessACL returns a fullaccess ACL
func (a *ACL) getFullaccessACL() ACL {
	return ACL{
		Fullaccess: true,
		LabelFilter: metricsql.LabelFilter{
			Label:      "namespace",
			Value:      ".*",
			IsRegexp:   true,
			IsNegative: false,
		},
		RawACL: ".*",
	}
}

// loadACL loads ACL from a file
func (app *application) loadACL(path string) (ACLMap, error) {
	aclMap := make(ACLMap)

	yamlFile, err := os.ReadFile(path)
	if err != nil {
		return aclMap, err
	}
	var aclYaml map[string]string

	err = yaml.Unmarshal(yamlFile, &aclYaml)
	if err != nil {
		return aclMap, err
	}

	for role, rawACL := range aclYaml {
		acl := ACL{}

		acl, err := acl.PrepareACL(rawACL)
		if err != nil {
			return ACLMap{}, err
		}

		aclMap[role] = acl
	}

	return aclMap, nil
}
