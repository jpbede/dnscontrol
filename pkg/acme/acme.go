// Package acme provides a means of performing Let's Encrypt DNS challenges via a DNSConfig
package acme

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/StackExchange/dnscontrol/v4/models"
	"github.com/StackExchange/dnscontrol/v4/pkg/nameservers"
	"github.com/StackExchange/dnscontrol/v4/pkg/notifications"
	"github.com/StackExchange/dnscontrol/v4/pkg/zonerecs"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	acmelog "github.com/go-acme/lego/v4/log"
)

// CertConfig describes a certificate's configuration.
type CertConfig struct {
	CertName   string   `json:"cert_name"`
	Names      []string `json:"names"`
	UseECC     bool     `json:"use_ecc"`
	MustStaple bool     `json:"must_staple"`
}

// Client is an interface for systems that issue or renew certs.
type Client interface {
	IssueOrRenewCert(config *CertConfig, renewUnder int, verbose bool) (bool, error)
}

type certManager struct {
	email         string
	acmeDirectory string
	acmeHost      string

	storage         Storage
	cfg             *models.DNSConfig
	domains         map[string]*models.DomainConfig
	originalDomains []*models.DomainConfig

	notifier notifications.Notifier

	account    *Account
	waitedOnce bool
}

const (
	// LetsEncryptLive is the endpoint for updates (production).
	LetsEncryptLive = "https://acme-v02.api.letsencrypt.org/directory"
	// LetsEncryptStage is the endpoint for the staging area.
	LetsEncryptStage = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// New is a factory for acme clients.
func New(cfg *models.DNSConfig, directory string, email string, server string, notify notifications.Notifier) (Client, error) {
	return commonNew(cfg, directoryStorage(directory), email, server, notify)
}

func commonNew(cfg *models.DNSConfig, storage Storage, email string, server string, notify notifications.Notifier) (Client, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("ACME directory '%s' is not a valid URL", server)
	}
	c := &certManager{
		storage:       storage,
		email:         email,
		acmeDirectory: server,
		acmeHost:      u.Host,
		cfg:           cfg,
		domains:       map[string]*models.DomainConfig{},
		notifier:      notify,
	}

	acct, err := c.getOrCreateAccount()
	if err != nil {
		return nil, err
	}
	c.account = acct
	return c, nil
}

// NewVault is a factory for new vaunt clients.
func NewVault(cfg *models.DNSConfig, vaultPath string, email string, server string, notify notifications.Notifier) (Client, error) {
	storage, err := makeVaultStorage(vaultPath)
	if err != nil {
		return nil, err
	}
	return commonNew(cfg, storage, email, server, notify)
}

// IssueOrRenewCert will obtain a certificate with the given name if it does not exist,
// or renew it if it is close enough to the expiration date.
// It will return true if it issued or updated the certificate.
func (c *certManager) IssueOrRenewCert(cfg *CertConfig, renewUnder int, verbose bool) (bool, error) {
	if !verbose {
		acmelog.Logger = log.New(io.Discard, "", 0)
	}
	defer c.finalCleanUp() //nolint:errcheck

	log.Printf("Checking certificate [%s]", cfg.CertName)
	existing, err := c.storage.GetCertificate(cfg.CertName)
	if err != nil {
		return false, err
	}

	var client *lego.Client

	action := func() (*certificate.Resource, error) {
		return client.Certificate.Obtain(certificate.ObtainRequest{
			Bundle:     true,
			Domains:    cfg.Names,
			MustStaple: cfg.MustStaple,
		})
	}

	if existing == nil {
		log.Println("No existing cert found. Issuing new...")
	} else {
		names, daysLeft, err := getCertInfo(existing.Certificate)
		if err != nil {
			return false, err
		}
		log.Printf("Found existing cert. %0.2f days remaining.", daysLeft)
		namesOK := dnsNamesEqual(cfg.Names, names)
		if daysLeft >= float64(renewUnder) && namesOK {
			log.Println("Nothing to do")
			// nothing to do
			return false, nil
		}
		if !namesOK {
			log.Println("DNS Names don't match expected set. Reissuing.")
		} else {
			log.Println("Renewing cert")
			action = func() (*certificate.Resource, error) {
				return client.Certificate.Renew(*existing, true, cfg.MustStaple, "")
			}
		}
	}

	kt := certcrypto.RSA2048
	if cfg.UseECC {
		kt = certcrypto.EC256
	}
	config := lego.NewConfig(c.account)
	config.CADirURL = c.acmeDirectory
	config.Certificate.KeyType = kt
	client, err = lego.NewClient(config)
	if err != nil {
		return false, err
	}
	client.Challenge.Remove(challenge.HTTP01)
	client.Challenge.Remove(challenge.TLSALPN01)
	client.Challenge.SetDNS01Provider(c, dns01.WrapPreCheck(c.preCheckDNS)) //nolint:errcheck

	certResource, err := action()
	if err != nil {
		return false, err
	}
	fmt.Printf("Obtained certificate for %s\n", cfg.CertName)
	if err = c.storage.StoreCertificate(cfg.CertName, certResource); err != nil {
		return true, err
	}

	return true, nil
}

func getCertInfo(pemBytes []byte) (names []string, remaining float64, err error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, 0, errors.New("invalid certificate PEM data")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, 0, err
	}
	daysLeft := float64(time.Until(cert.NotAfter)) / float64(time.Hour*24)
	return cert.DNSNames, daysLeft, nil
}

// checks two lists of sans to make sure they have all the same names in them.
func dnsNamesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i, s := range a {
		if b[i] != s {
			return false
		}
	}
	return true
}

func (c *certManager) Present(domain, token, keyAuth string) (e error) {
	d := c.cfg.DomainContainingFQDN(domain)
	name := d.Name
	if seen := c.domains[name]; seen != nil {
		// we've already pre-processed this domain, just need to add to it.
		d = seen
	} else {
		// one-time tasks to get this domain ready.
		// if multiple validations on a single domain, we don't need to rebuild all this.

		// fix NS records for this domain's DNS providers
		nsList, err := nameservers.DetermineNameservers(d)
		if err != nil {
			return err
		}
		d.Nameservers = nsList
		nameservers.AddNSRecords(d)

		// make sure we have the latest config before we change anything.
		// alternately, we could avoid a lot of this trouble if we really trusted no-purge in all cases
		if err := c.ensureNoPendingCorrections(d); err != nil {
			return err
		}

		// Copy domain and work from cpy from now on. That way original config can be used to "restore" when we are all done.
		cpy, err := d.Copy()
		if err != nil {
			return err
		}
		c.originalDomains = append(c.originalDomains, d)
		c.domains[name] = cpy
		d = cpy
	}

	fqdn, val := dns01.GetRecord(domain, keyAuth)
	txt := &models.RecordConfig{Type: "TXT"}
	if err := txt.SetTargetTXT(val); err != nil {
		return err
	}
	txt.SetLabelFromFQDN(fqdn, d.Name)
	d.Records = append(d.Records, txt)
	return c.getAndRunCorrections(d)
}

func (c *certManager) ensureNoPendingCorrections(d *models.DomainConfig) error {
	corrections, err := c.getCorrections(d)
	if err != nil {
		return err
	}
	if len(corrections) != 0 {
		// TODO: maybe allow forcing through this check.
		for _, c := range corrections {
			fmt.Println(c.Msg)
		}
		return fmt.Errorf("found %d pending corrections for %s. Not going to proceed issuing certificates", len(corrections), d.Name)
	}
	return nil
}

// IgnoredProviders is a list of provider names that should not be used to fill challenges.
var IgnoredProviders = map[string]bool{}

func (c *certManager) getCorrections(d *models.DomainConfig) ([]*models.Correction, error) {
	cs := []*models.Correction{}
	for _, p := range d.DNSProviderInstances {
		if IgnoredProviders[p.Name] {
			continue
		}
		dc, err := d.Copy()
		if err != nil {
			return nil, err
		}
		reports, corrections, _, err := zonerecs.CorrectZoneRecords(p.Driver, dc)
		if err != nil {
			return nil, err
		}
		for _, c := range reports {
			c.Msg = fmt.Sprintf("INFO[%s] %s", p.Name, strings.TrimSpace(c.Msg))
		}
		for _, c := range corrections {
			c.Msg = fmt.Sprintf("[%s] %s", p.Name, strings.TrimSpace(c.Msg))
		}
		cs = append(cs, corrections...)
	}
	return cs, nil
}

func (c *certManager) getAndRunCorrections(d *models.DomainConfig) error {
	cs, err := c.getCorrections(d)
	if err != nil {
		return err
	}
	fmt.Printf("%d corrections\n", len(cs))
	for _, corr := range cs {
		fmt.Printf("Running [%s]\n", corr.Msg)
		err = corr.F()
		err2 := c.notifier.Notify(d.Name, "certs", corr.Msg, err, false)
		if err != nil {
			return err
		}
		if err2 != nil {
			return err2
		}
	}
	return nil
}

func (c *certManager) CleanUp(domain, token, keyAuth string) error {
	// do nothing for now. We will do a final clean up step at the very end.
	return nil
}

func (c *certManager) finalCleanUp() error {
	log.Println("Cleaning up all records we made")
	var lastError error
	for _, d := range c.originalDomains {
		if err := c.getAndRunCorrections(d); err != nil {
			log.Printf("ERROR cleaning up: %s", err)
			lastError = err
		}
	}
	return lastError
}
