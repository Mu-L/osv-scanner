package helper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/osv-scanner/v2/internal/cmdlogger"
	"github.com/google/osv-scanner/v2/internal/reporter"
	"github.com/google/osv-scanner/v2/internal/spdx"
	"github.com/google/osv-scanner/v2/pkg/models"
	"github.com/google/osv-scanner/v2/pkg/osvscanner"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// OfflineFlags is a map of flags which require network access to operate,
// with the values to set them to in order to disable them
var OfflineFlags = map[string]string{
	"offline-vulnerabilities": "true",
	"no-resolve":              "true",
}

// sets default port(8000) as a global variable
var (
	servePort = "8000" // default port
)

// a "boolean or list" flag whose presence indicates a summary of licenses should
// be printed, and whose (optional) value will be a comma-delimited list of licenses
// that should be considered allowed
type allowedLicencesFlag struct {
	allowlist []string
}

func (g *allowedLicencesFlag) Get() any {
	return g
}

func (g *allowedLicencesFlag) Set(value string) error {
	if value == "" || value == "false" || value == "true" {
		g.allowlist = nil
	} else {
		g.allowlist = strings.Split(value, ",")
	}

	return nil
}

// IsBoolFlag indicates that it is valid to use this flag in a boolean context
// and is what lets us accept both enable/disable and list-of-licenses values
func (g *allowedLicencesFlag) IsBoolFlag() bool {
	return true
}

func (g *allowedLicencesFlag) String() string {
	return strings.Join(g.allowlist, ",")
}

func GetScanGlobalFlags(defaultExtractors []string) []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:      "config",
			Usage:     "set/override config file",
			TakesFile: true,
		},
		&cli.StringFlag{
			Name:    "format",
			Aliases: []string{"f"},
			Usage:   "sets the output format; value can be: " + strings.Join(reporter.Format(), ", "),
			Value:   "table",
			Action: func(_ context.Context, _ *cli.Command, s string) error {
				if slices.Contains(reporter.Format(), s) {
					if s != "vertical" && s != "table" && s != "markdown" {
						cmdlogger.SendEverythingToStderr()
					}

					return nil
				}

				return fmt.Errorf("unsupported output format \"%s\" - must be one of: %s", s, strings.Join(reporter.Format(), ", "))
			},
		},
		&cli.BoolFlag{
			Name:  "serve",
			Usage: "output as HTML result and serve it locally",
		},
		&cli.StringFlag{
			Name:  "port",
			Usage: "port number to use when serving HTML report (default: 8000)",
			Action: func(_ context.Context, _ *cli.Command, p string) error {
				servePort = p
				return nil
			},
		},
		&cli.StringFlag{
			Name:      "output",
			Usage:     "saves the result to the given file path",
			TakesFile: true,
		},
		&cli.StringFlag{
			Name:  "verbosity",
			Usage: "specify the level of information that should be provided during runtime; value can be: " + strings.Join(cmdlogger.Levels(), ", "),
			Value: "info",
			Action: func(_ context.Context, _ *cli.Command, s string) error {
				lvl, err := cmdlogger.ParseLevel(s)

				if err != nil {
					return err
				}

				cmdlogger.SetLevel(lvl)

				return nil
			},
		},
		&cli.BoolFlag{
			Name:  "offline",
			Usage: "run in offline mode, disabling any features requiring network access",
			Action: func(_ context.Context, cmd *cli.Command, b bool) error {
				if !b {
					return nil
				}
				// Disable the features requiring network access.
				for flag, value := range OfflineFlags {
					// TODO(michaelkedar): do something if the flag was already explicitly set.

					// Skip setting the flag if the current command doesn't have it.
					if !slices.ContainsFunc(cmd.Flags, func(f cli.Flag) bool {
						return slices.Contains(f.Names(), flag)
					}) {
						continue
					}

					if err := cmd.Set(flag, value); err != nil {
						panic(fmt.Sprintf("failed setting offline flag %s to %s: %v", flag, value, err))
					}
				}

				return nil
			},
		},
		&cli.BoolFlag{
			Name:  "offline-vulnerabilities",
			Usage: "checks for vulnerabilities using local databases that are already cached",
		},
		&cli.BoolFlag{
			Name:  "download-offline-databases",
			Usage: "downloads vulnerability databases for offline comparison",
		},
		&cli.StringFlag{
			Name:   "local-db-path",
			Usage:  "sets the path that local databases should be stored",
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:  "no-resolve",
			Usage: "disable transitive dependency resolution of manifest files",
		},
		&cli.BoolFlag{
			Name:  "allow-no-lockfiles",
			Usage: "has the scanner consider no lockfiles being found as ok",
		},
		&cli.BoolFlag{
			Name:  "all-packages",
			Usage: "when json output is selected, prints all packages",
		},
		&cli.BoolFlag{
			Name:  "all-vulns",
			Usage: "show all vulnerabilities including unimportant and uncalled ones",
		},
		&cli.GenericFlag{
			Name:  "licenses",
			Usage: "report on licenses based on an allowlist",
			Value: &allowedLicencesFlag{},
		},
		&cli.StringSliceFlag{
			Name:  "experimental-extractors",
			Usage: "list of specific extractors and ExtractorPresets of extractors to use",
			Value: defaultExtractors,
		},
		&cli.StringSliceFlag{
			Name:  "experimental-disable-extractors",
			Usage: "list of specific extractors and ExtractorPresets of extractors to not use",
		},
	}
}

// ServeHTML serves the single HTML file for remote accessing.
// The program will keep running to serve the HTML report on localhost
// until the user manually terminates it (e.g. using Ctrl+C).
func ServeHTML(outputPath string) {
	localhostURL := fmt.Sprintf("http://localhost:%s/", servePort)
	cmdlogger.Infof("Serving HTML report at %s", localhostURL)
	cmdlogger.Infof("If you are accessing remotely, use the following SSH command:")
	cmdlogger.Infof("`ssh -L local_port:destination_server_ip:%s ssh_server_hostname`", servePort)
	server := &http.Server{
		Addr: ":" + servePort,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, outputPath)
		}),
		ReadHeaderTimeout: 3 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		cmdlogger.Errorf("Failed to start server: %v", err)
	}
}

func PrintResult(stdout, stderr io.Writer, outputPath, format string, diffVulns *models.VulnerabilityResults, showAllVulns bool) error {
	termWidth := 0
	var err error
	if outputPath != "" { // Output is definitely a file
		stdout, err = os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("failed to create output file: %w", err)
		}
	} else { // Output might be a terminal
		if stdoutAsFile, ok := stdout.(*os.File); ok {
			termWidth, _, err = term.GetSize(int(stdoutAsFile.Fd()))
			if err != nil { // If output is not a terminal,
				termWidth = 0
			}
		}
	}

	writer := stdout

	if format == "gh-annotations" {
		writer = stderr
	}

	return reporter.PrintResult(diffVulns, format, writer, termWidth, showAllVulns)
}

func GetScanLicensesAllowlist(cmd *cli.Command) ([]string, error) {
	if !cmd.IsSet("licenses") {
		return []string{}, nil
	}

	allowlist := cmd.Generic("licenses").(*allowedLicencesFlag).allowlist

	if len(allowlist) == 0 {
		return []string{}, nil
	}

	if unrecognized := spdx.Unrecognized(allowlist); len(unrecognized) > 0 {
		return nil, fmt.Errorf("--licenses requires comma-separated spdx licenses. The following license(s) are not recognized as spdx: %s", strings.Join(unrecognized, ","))
	}

	if cmd.Bool("offline") {
		allowlist = []string{}
	}

	return allowlist, nil
}

func GetCommonScannerActions(cmd *cli.Command, scanLicensesAllowlist []string) osvscanner.ScannerActions {
	return osvscanner.ScannerActions{
		IncludeGitRoot:     cmd.Bool("include-git-root"),
		ConfigOverridePath: cmd.String("config"),
		ShowAllPackages:    cmd.Bool("all-packages"),
		ShowAllVulns:       cmd.Bool("all-vulns"),

		CompareOffline:        cmd.Bool("offline-vulnerabilities"),
		DownloadDatabases:     cmd.Bool("download-offline-databases"),
		LocalDBPath:           cmd.String("local-db-path"),
		ScanLicensesSummary:   cmd.IsSet("licenses"),
		ScanLicensesAllowlist: scanLicensesAllowlist,
	}
}

func GetExperimentalScannerActions(cmd *cli.Command) osvscanner.ExperimentalScannerActions {
	return osvscanner.ExperimentalScannerActions{
		Extractors: ResolveEnabledExtractors(
			cmd.StringSlice("experimental-extractors"),
			cmd.StringSlice("experimental-disable-extractors"),
		),
	}
}
