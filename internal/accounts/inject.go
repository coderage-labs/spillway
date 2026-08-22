package accounts

import "net/http"

// BearerInjector returns a request injector enforcing design-doc §4's only
// allowed auth mutation: drop any client-sent x-api-key and set
// Authorization: Bearer <token>. Everything else is untouched — claude's own
// headers (anthropic-beta, anthropic-version, user-agent, ...) pass through.
func BearerInjector(token string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Del("X-Api-Key")
		r.Header.Set("Authorization", "Bearer "+token)
	}
}
