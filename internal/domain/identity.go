// Package domain contains application data without transport or SDK dependencies.
package domain

// Scope is supplied by trusted authentication, never by an unverified request body.
// All resource access must be constrained by both fields.
type Scope struct {
	TenantID string
	UserID   string
}
