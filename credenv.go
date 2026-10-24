package main

import (
	"regexp"
	"strings"
)

// credentialEnvName judges, by name, which inherited variables are credentials
// or channels to one. It is for what a person or a tool exported for its own
// purposes (a cloud key, an API token, an ssh-agent socket); a sandboxed agent
// that is not meant to have them (AgentConfig.CleanEnv) is run without.
var (
	credentialWord    = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|CREDENTIALS?|API_?KEY|ACCESS_?KEY|SECRET_?KEY|PRIVATE_?KEY|AUTH_?KEY)(_|$)`)
	gitConfigInjected = regexp.MustCompile(`^GIT_CONFIG_(KEY|VALUE)_[0-9]+$`)
	// Providers whose whole variable family is credentials or the account they select.
	credentialPrefixes = []string{"AWS_", "AZURE_", "GOOGLE_APPLICATION_", "GCLOUD_", "CLOUDSDK_", "GITHUB_", "GITLAB_", "GH_", "VAULT_", "OKTA_"}
	// Channels to credentials that carry no secret in their name.
	credentialExact = map[string]bool{
		"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "GPG_AGENT_INFO": true,
		"GIT_ASKPASS": true, "SSH_ASKPASS": true, "GIT_CONFIG_COUNT": true,
	}
)

func credentialEnvName(name string) bool {
	if credentialExact[name] || gitConfigInjected.MatchString(name) || credentialWord.MatchString(name) {
		return true
	}
	for _, p := range credentialPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// withoutCredentialEnv is env minus the variables credentialEnvName names, in order.
func withoutCredentialEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if credentialEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
