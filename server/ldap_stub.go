//go:build !ldap

package main

// Stubs for builds without LDAP (the default). The real implementation lives
// in ldap.go behind the `ldap` build tag, so a default production binary
// neither links go-ldap nor carries directory logic, while the code stays
// ready to enable when a deployment needs it.

func ldapEnabled() bool                                        { return false }
func ldapAuthenticate(username, password string) (bool, error) { return false, nil }
func ensureLDAPUser(username string) error                     { return nil }
