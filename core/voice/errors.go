package voice

import "fmt"

// configError reports a provider that is selected but missing a required
// credential or command. It never carries the secret value itself.
type configError struct {
	provider string
	missing  string
}

func (e *configError) Error() string {
	return fmt.Sprintf("voice: provider %q requires %s", e.provider, e.missing)
}

// unknownProviderError reports a VOICE_PROVIDER value that is not supported.
type unknownProviderError struct {
	provider string
}

func (e *unknownProviderError) Error() string {
	return fmt.Sprintf("voice: unknown provider %q (want mistral|openai|local)", e.provider)
}
