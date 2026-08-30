//go:build ldap

package main

// LDAP / Active Directory authentication.
//
// Active only when LDAP_URL is set; otherwise the module is inert and sign-in
// keeps working with local username and password.
//
// Two modes, selected by which variables are present:
//
//  1. Direct bind — LDAP_USER_DN_TEMPLATE contains "%s":
//     AD (UPN):      LDAP_USER_DN_TEMPLATE="%s@corp.example"
//     AD (DOMAIN\):  LDAP_USER_DN_TEMPLATE="CORP\\%s"
//     OpenLDAP:      LDAP_USER_DN_TEMPLATE="uid=%s,ou=people,dc=corp,dc=example"
//     Sign-in binds that DN with the supplied password. Simplest option and
//     needs no service account.
//
//  2. Search + bind — LDAP_BIND_DN (with LDAP_BIND_PASSWORD): a service
//     account looks the user up under LDAP_BASE_DN using LDAP_USER_FILTER,
//     then binds the resulting DN. Allows signing in by sAMAccountName or
//     mail without knowing the DN layout.
//
// Also:
//
//	LDAP_START_TLS=true   StartTLS over ldap:// (389)
//	LDAP_SKIP_VERIFY=true skip TLS verification (test or private PKI only)
//	LDAP_USER_FILTER      defaults to "(sAMAccountName=%s)"

import (
	"crypto/tls"
	"fmt"
	"os"

	"github.com/go-ldap/ldap/v3"
)

type ldapConfig struct {
	url            string
	userDNTemplate string
	baseDN         string
	bindDN         string
	bindPassword   string
	userFilter     string
	startTLS       bool
	skipVerify     bool
}

var ldapCfg = func() *ldapConfig {
	url := os.Getenv("LDAP_URL")
	if url == "" {
		return nil
	}
	return &ldapConfig{
		url:            url,
		userDNTemplate: os.Getenv("LDAP_USER_DN_TEMPLATE"),
		baseDN:         os.Getenv("LDAP_BASE_DN"),
		bindDN:         os.Getenv("LDAP_BIND_DN"),
		bindPassword:   os.Getenv("LDAP_BIND_PASSWORD"),
		userFilter:     getEnvOrDefault("LDAP_USER_FILTER", "(sAMAccountName=%s)"),
		startTLS:       os.Getenv("LDAP_START_TLS") == "true",
		skipVerify:     os.Getenv("LDAP_SKIP_VERIFY") == "true",
	}
}()

func ldapEnabled() bool { return ldapCfg != nil }

// ldapAuthenticate checks a username and password against the directory.
// (true, nil) means valid, (false, nil) invalid, and (false, err) a network
// or configuration failure — which callers must not treat as a wrong
// password, because it is a directory outage.
func ldapAuthenticate(username, password string) (bool, error) {
	if ldapCfg == nil {
		return false, nil
	}
	// LDAP allows an "unauthenticated bind", where an empty password
	// succeeds, so empty passwords are rejected before any bind.
	if password == "" {
		return false, nil
	}

	tlsConf := &tls.Config{InsecureSkipVerify: ldapCfg.skipVerify} // #nosec G402 — gated by LDAP_SKIP_VERIFY
	conn, err := ldap.DialURL(ldapCfg.url, ldap.DialWithTLSConfig(tlsConf))
	if err != nil {
		return false, fmt.Errorf("ldap dial: %w", err)
	}
	defer conn.Close()

	if ldapCfg.startTLS {
		if err := conn.StartTLS(tlsConf); err != nil {
			return false, fmt.Errorf("ldap starttls: %w", err)
		}
	}

	userDN, err := ldapResolveUserDN(conn, username)
	if err != nil {
		return false, err
	}
	if userDN == "" {
		return false, nil // no such user
	}

	// Binding as the user is the password check.
	if err := conn.Bind(userDN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return false, nil
		}
		return false, fmt.Errorf("ldap bind: %w", err)
	}
	return true, nil
}

// ldapResolveUserDN returns the user's DN, either from the template or via a
// search performed by the service account.
func ldapResolveUserDN(conn *ldap.Conn, username string) (string, error) {
	if ldapCfg.userDNTemplate != "" {
		return fmt.Sprintf(ldapCfg.userDNTemplate, username), nil
	}
	if ldapCfg.bindDN == "" {
		return "", fmt.Errorf("ldap misconfigured: set LDAP_USER_DN_TEMPLATE or LDAP_BIND_DN")
	}
	// Search + bind.
	if err := conn.Bind(ldapCfg.bindDN, ldapCfg.bindPassword); err != nil {
		return "", fmt.Errorf("ldap service bind: %w", err)
	}
	req := ldap.NewSearchRequest(
		ldapCfg.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
		fmt.Sprintf(ldapCfg.userFilter, ldap.EscapeFilter(username)),
		[]string{"dn"}, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return "", fmt.Errorf("ldap search: %w", err)
	}
	if len(res.Entries) != 1 {
		return "", nil // zero or ambiguous match counts as not found
	}
	return res.Entries[0].DN, nil
}

// ensureLDAPUser creates the local record for a directory user if it is
// missing. The stored password hash is deliberately unusable ("!ldap"), so
// such accounts can only ever sign in through the directory.
func ensureLDAPUser(username string) error {
	_, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,$2)
	                   ON CONFLICT (username) DO NOTHING`, username, "!ldap")
	return err
}
