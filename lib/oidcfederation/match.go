package oidcfederation

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

func normalizeMatch(m *Match, issuer IssuerSpec) error {
	if len(m.Subject) > 1024 || len(m.SubjectPrefix) > 1024 || len(m.Claims) > 32 {
		return ErrInvalid
	}
	for key, value := range m.Claims {
		if _, err := pointerParts(key); err != nil || value == "" || len(value) > 1024 {
			return fmt.Errorf("%w: invalid claim condition", ErrInvalid)
		}
	}
	// Each matcher carries its provider's mandatory identity checks, so a rule
	// cannot choose a weaker one than its issuer's.
	if m.Type != issuer.PresetType {
		return fmt.Errorf("%w: the matcher type must be the issuer's type, %s", ErrInvalid, issuer.PresetType)
	}
	if m.Type != "github_actions" && (m.RepositoryID != "" || m.RepositoryOwnerID != "" || m.Workflow != "" || m.WorkflowRef != "" || m.JobWorkflowRef != "" || m.Ref != "" || m.Environment != "" || len(m.EventNames) != 0) {
		return fmt.Errorf("%w: GitHub conditions require the GitHub matcher", ErrInvalid)
	}
	if m.Type != "kubernetes" && (m.Namespace != "" || m.ServiceAccount != "") {
		return fmt.Errorf("%w: Kubernetes conditions require the Kubernetes matcher", ErrInvalid)
	}
	switch m.Type {
	case "github_actions":
		if issuer.IssuerURL != GitHubIssuer || !numericID.MatchString(m.RepositoryID) || !numericID.MatchString(m.RepositoryOwnerID) || (m.Workflow == "" && m.WorkflowRef == "" && m.JobWorkflowRef == "") {
			return fmt.Errorf("%w: repository/owner IDs and a workflow restriction are required", ErrInvalid)
		}
		if m.Workflow != "" && (!strings.HasPrefix(m.Workflow, ".github/workflows/") || path.Clean(m.Workflow) != m.Workflow || len(m.Workflow) > 512) {
			return fmt.Errorf("%w: invalid workflow path", ErrInvalid)
		}
		if m.EventNames == nil {
			m.EventNames = []string{"push", "workflow_dispatch"}
		}
		if len(m.EventNames) == 0 || len(m.EventNames) > 20 {
			return fmt.Errorf("%w: event restrictions are required", ErrInvalid)
		}
	case "kubernetes":
		if !kubeLabel.MatchString(m.Namespace) || len(m.Namespace) > 63 || len(m.ServiceAccount) > 253 || m.ServiceAccount == "" {
			return fmt.Errorf("%w: Kubernetes namespace and service account are required", ErrInvalid)
		}
		for _, label := range strings.Split(m.ServiceAccount, ".") {
			if !kubeLabel.MatchString(label) || len(label) > 63 {
				return ErrInvalid
			}
		}
	case "google_cloud":
		if !numericID.MatchString(m.Subject) {
			return fmt.Errorf("%w: numeric Google workload subject is required", ErrInvalid)
		}
	case "microsoft_entra":
		if m.Subject == "" && m.Claims["/oid"] == "" && m.Claims["/appid"] == "" && m.Claims["/azp"] == "" {
			return fmt.Errorf("%w: an Entra workload identity restriction is required", ErrInvalid)
		}
		if m.Claims == nil {
			m.Claims = make(map[string]string)
		}
		if tenant := entraTenant(issuer.IssuerURL); tenant != "" {
			if value, exists := m.Claims["/tid"]; exists && value != tenant {
				return fmt.Errorf("%w: tenant claim disagrees with issuer", ErrInvalid)
			}
			m.Claims["/tid"] = tenant
		}
		// A client ID names the app a token was issued through, not who it acts
		// for. Without a pinned object ID, accept only tokens Entra marks as the
		// app's own.
		if m.Subject == "" && m.Claims["/oid"] == "" {
			if value, exists := m.Claims["/idtyp"]; exists && value != "app" {
				return fmt.Errorf("%w: a rule matched by client ID accepts only app tokens", ErrInvalid)
			}
			m.Claims["/idtyp"] = "app"
		}
	case "aws", "custom_oidc":
		if m.Subject == "" && m.SubjectPrefix == "" && len(m.Claims) == 0 {
			return fmt.Errorf("%w: workload restrictions are required", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported matcher", ErrInvalid)
	}
	return nil
}

// Matches evaluates an already validated matcher against verified claims.
func (m Match) Matches(claims map[string]any) bool {
	subject, ok := claims["sub"].(string)
	if !ok || (m.Subject != "" && subject != m.Subject) || (m.SubjectPrefix != "" && !strings.HasPrefix(subject, m.SubjectPrefix)) {
		return false
	}
	for key, expected := range m.Claims {
		value, ok := ClaimString(claims, key)
		if !ok || value != expected {
			return false
		}
	}
	switch m.Type {
	case "github_actions":
		if claims["repository_id"] != m.RepositoryID || claims["repository_owner_id"] != m.RepositoryOwnerID || !slices.Contains(m.EventNames, stringClaim(claims, "event_name")) {
			return false
		}
		if (m.Ref != "" && stringClaim(claims, "ref") != m.Ref) || (m.Environment != "" && stringClaim(claims, "environment") != m.Environment) || (m.WorkflowRef != "" && stringClaim(claims, "workflow_ref") != m.WorkflowRef) || (m.JobWorkflowRef != "" && stringClaim(claims, "job_workflow_ref") != m.JobWorkflowRef) {
			return false
		}
		if m.Workflow != "" {
			repository, ref := stringClaim(claims, "repository"), stringClaim(claims, "ref")
			return repository != "" && ref != "" && stringClaim(claims, "workflow_ref") == repository+"/"+m.Workflow+"@"+ref
		}
		return true
	case "kubernetes":
		return subject == "system:serviceaccount:"+m.Namespace+":"+m.ServiceAccount
	case "microsoft_entra":
		// A token issued to a signed-in user carries delegated scopes. Only an
		// application's own token stands for a workload.
		_, delegated := claims["scp"]
		return !delegated
	case "aws", "google_cloud", "custom_oidc":
		return true
	default:
		return false
	}
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return value
}
