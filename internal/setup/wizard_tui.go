package setup

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
)

// The wizard as it looks at a real terminal: one question on screen at a time,
// arrow keys to choose, and only the follow-up questions a choice actually
// needs. Everything here is presentation — every answer lands through the same
// apply* functions the typed path uses, so the two can't disagree about what a
// choice means, only about how it was asked.
//
// Each question is its own form rather than one long one, because the follow-ups
// are conditional: you only name a domain if you said it lives on the internet,
// and only give S3 credentials if you said S3. Running them in sequence keeps
// that plain, and matches the walk-through shape the wizard has always had.

// wizardTUI runs the four questions and writes the answers into fc.
func wizardTUI(fc *FileConfig, dataDir string) error {
	fmt.Println()
	for _, step := range []func(*FileConfig, string) error{
		tuiReachability,
		tuiPort,
		tuiDatabase,
		tuiMedia,
	} {
		if err := step(fc, dataDir); err != nil {
			return err
		}
	}
	return nil
}

// run executes one form, translating an abandoned wizard into ErrCancelled so
// callers don't have to know what library asked the question.
func run(fields ...huh.Field) error {
	err := huh.NewForm(huh.NewGroup(fields...)).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrCancelled
	}
	return err
}

// tuiReachability asks where the server lives, then names the domain if it's on
// the internet. Both answers are private servers — you hand out the address
// either way — so the difference the question is really drawing out is whether
// the address is a LAN IP or a name with HTTPS behind it.
func tuiReachability(fc *FileConfig, _ string) error {
	choice := reachOf(fc)
	if err := run(
		huh.NewSelect[string]().
			Title("Where are you running this?").
			Description("Either way it's private — people need the address you give them.").
			Options(
				huh.NewOption("A computer on my network", reachNetwork),
				huh.NewOption("A server with a domain name", reachDomain),
			).
			Value(&choice),
	); err != nil {
		return err
	}

	if choice != reachDomain {
		applyReachability(fc, choice, "")
		return nil
	}

	// Bombers deliberately does NOT terminate TLS itself. It could — Go ships
	// the pieces — but certificates are issued against ports 80 and 443, and
	// binding those on Linux needs root. Nothing else in the install path needs
	// sudo, and a reverse proxy is already a system service holding exactly
	// those privileges. So: one more program on a VPS, and this stays an
	// ordinary unprivileged process. Setup prints the Caddyfile when it's done.
	domain := fc.Domain
	if err := run(
		huh.NewInput().
			Title("Domain").
			Description("Point an A record for it at this machine's IP first — that DNS\nentry IS the proof of ownership the certificate authority checks.").
			Placeholder(exampleDomain).
			Value(&domain).
			Validate(func(s string) error {
				if cleanDomain(s) == "" {
					return errors.New("a domain is required for this option")
				}
				return nil
			}),
	); err != nil {
		return err
	}

	applyReachability(fc, reachDomain, domain)
	if w := domainWarning(fc.Domain); w != "" {
		fmt.Println("  ! Note: " + w)
	}
	return nil
}

// tuiPort asks for the HTTP port.
func tuiPort(fc *FileConfig, _ string) error {
	port := firstNonEmpty(fc.Port, defaultPort)
	if err := run(huh.NewInput().Title("Port").Value(&port)); err != nil {
		return err
	}
	fc.Port = normalizePort(port, fc.Port)
	return nil
}

// tuiDatabase asks where the data lives, then for a URL if it's not ours.
func tuiDatabase(fc *FileConfig, _ string) error {
	backend := "embedded"
	if fc.DBBackend == "external" {
		backend = "external"
	}
	if err := run(
		huh.NewSelect[string]().
			Title("Where should Bombers keep its data?").
			Options(
				huh.NewOption("Run Postgres for me — nothing to install", "embedded"),
				huh.NewOption("Use my own Postgres — I'll give a connection URL", "external"),
			).
			Value(&backend),
	); err != nil {
		return err
	}

	if backend == "external" {
		url := firstNonEmpty(fc.DatabaseURL, defaultDatabaseURL)
		if err := run(huh.NewInput().Title("Postgres URL").Value(&url)); err != nil {
			return err
		}
		applyExternalDB(fc, url)
		return nil
	}

	if w := applyEmbeddedPG(fc); w != "" {
		fmt.Println("  ! Note: " + w)
	}
	return nil
}

// tuiMedia asks where uploads live, then for credentials if they're not local.
func tuiMedia(fc *FileConfig, dataDir string) error {
	backend := "fs"
	if fc.MediaBackend == "s3" {
		backend = "s3"
	}
	if err := run(
		huh.NewSelect[string]().
			Title("Where should uploaded media go?").
			Description("Avatars and banners.").
			Options(
				huh.NewOption("Files on this computer — nothing extra to run", "fs"),
				huh.NewOption("An S3 or MinIO server", "s3"),
			).
			Value(&backend),
	); err != nil {
		return err
	}

	if backend != "s3" {
		applyFilesystemMedia(fc, dataDir)
		return nil
	}

	endpoint := firstNonEmpty(fc.S3Endpoint, defaultS3Endpoint)
	accessKey := firstNonEmpty(fc.S3AccessKey, defaultS3AccessKey)
	secretKey := firstNonEmpty(fc.S3SecretKey, defaultS3SecretKey)
	bucket := firstNonEmpty(fc.S3Bucket, defaultS3Bucket)
	useSSL := fc.S3UseSSL
	if err := run(
		huh.NewInput().Title("S3 endpoint").Value(&endpoint),
		huh.NewInput().Title("Access key").Value(&accessKey),
		// Masked because it ends up in a config file and on a screen someone
		// might be sharing; the typed path can't do this, which is one reason
		// the terminal path is the one people should get.
		huh.NewInput().Title("Secret key").EchoMode(huh.EchoModePassword).Value(&secretKey),
		huh.NewInput().Title("Bucket").Value(&bucket),
		huh.NewConfirm().Title("Use TLS for S3 (https)?").Value(&useSSL),
	); err != nil {
		return err
	}
	applyS3Media(fc, endpoint, accessKey, secretKey, bucket, useSSL)
	return nil
}
