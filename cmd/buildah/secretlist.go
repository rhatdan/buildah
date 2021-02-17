package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/report"
	"github.com/containers/common/pkg/secrets"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type listFlagType struct {
	format    string
	noHeading bool
}

type SecretRmOptions struct {
	All bool
}

type SecretCreateOptions struct {
	Driver string
}

type SecretInfoReport struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Spec      SecretSpec
}

var (
	listFlag      listFlagType
	rmOptions     SecretRmOptions
	createOptions SecretCreateOptions
	format        string
)

func init() {
	lsCmd := &cobra.Command{
		Use:     "ls [options]",
		Aliases: []string{"list"},
		Short:   "List secrets",
		RunE:    ls,
		Example: "podman secret ls",
		Args:    cobra.NoArgs,
		//ValidArgsFunction: completion.AutocompleteNone,
	}
	secretCommand.AddCommand(lsCmd)

	flags := lsCmd.Flags()
	formatFlagName := "format"
	flags.StringVar(&listFlag.format, formatFlagName, "{{.ID}}\t{{.Name}}\t{{.Driver}}\t{{.CreatedAt}}\t{{.UpdatedAt}}\t\n", "Format volume output using Go template")

	rmCmd := &cobra.Command{
		Use:   "rm [options] SECRET [SECRET...]",
		Short: "Remove one or more secrets",
		RunE:  rm,
		//		ValidArgsFunction: completion.AutocompleteSecrets,
		Example: "podman secret rm mysecret1 mysecret2",
	}
	flags = rmCmd.Flags()
	flags.BoolVarP(&rmOptions.All, "all", "a", false, "Remove all secrets")
	secretCommand.AddCommand(rmCmd)

	createCmd := &cobra.Command{
		Use:   "create [options] NAME FILE|-",
		Short: "Create a new secret",
		Long:  "Create a secret. Input can be a path to a file or \"-\" (read from stdin). Default driver is file (unencrypted).",
		RunE:  create,
		Args:  cobra.ExactArgs(2),
		Example: `podman secret create mysecret /path/to/secret
		printf "secretdata" | podman secret create mysecret -`,
		//ValidArgsFunction: completion.AutocompleteSecretCreate,
	}
	flags = createCmd.Flags()

	driverFlagName := "driver"
	flags.StringVar(&createOptions.Driver, driverFlagName, "file", "Specify secret driver")
	_ = createCmd.RegisterFlagCompletionFunc(driverFlagName, completion.AutocompleteNone)
	secretCommand.AddCommand(createCmd)

	inspectCmd := &cobra.Command{
		Use:     "inspect [options] SECRET [SECRET...]",
		Short:   "Inspect a secret",
		Long:    "Display detail information on one or more secrets",
		RunE:    inspect,
		Example: "podman secret inspect MYSECRET",
		Args:    cobra.MinimumNArgs(1),
		//ValidArgsFunction: completion.AutocompleteSecrets,
	}
	flags = inspectCmd.Flags()
	formatFlagName = "format"
	flags.StringVar(&format, formatFlagName, "", "Format volume output using Go template")
	//	_ = inspectCmd.RegisterFlagCompletionFunc(formatFlagName, completion.AutocompleteJSONFormat)

	secretCommand.AddCommand(inspectCmd)
}

type SecretListReport struct {
	ID        string
	Name      string
	Driver    string
	CreatedAt string
	UpdatedAt string
}

func secretsPath(c *cobra.Command) (string, error) {
	store, err := getStore(c)
	if err != nil {
		return "", err
	}
	return filepath.Join(store.GraphRoot(), "secrets"), nil
}

func secretsManager(c *cobra.Command) (*secrets.SecretsManager, error) {
	path, err := secretsPath(c)
	if err != nil {
		return nil, err
	}

	return secrets.NewManager(path)
}

func ls(cmd *cobra.Command, args []string) error {
	manager, err := secretsManager(cmd)
	if err != nil {
		return err
	}
	secretList, err := manager.List()
	if err != nil {
		return err
	}
	listed := make([]*SecretListReport, 0, len(secretList))
	for _, secret := range secretList {
		listed = append(listed, &SecretListReport{
			ID:        secret.ID,
			Name:      secret.Name,
			CreatedAt: units.HumanDuration(time.Since(secret.CreatedAt)) + " ago",
			UpdatedAt: units.HumanDuration(time.Since(secret.CreatedAt)) + " ago",
			Driver:    secret.Driver,
		})
	}
	return outputTemplate(cmd, listed)
}

func outputTemplate(cmd *cobra.Command, responses []*SecretListReport) error {
	headers := report.Headers(SecretListReport{}, map[string]string{
		"CreatedAt": "CREATED",
		"UpdatedAt": "UPDATED",
	})

	row := report.NormalizeFormat(listFlag.format)
	format := report.EnforceRange(row)

	tmpl, err := template.New("list secret").Parse(format)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 12, 2, 2, ' ', 0)
	defer w.Flush()

	if cmd.Flags().Changed("format") && !report.HasTable(listFlag.format) {
		listFlag.noHeading = true
	}

	if !listFlag.noHeading {
		if err := tmpl.Execute(w, headers); err != nil {
			return errors.Wrapf(err, "failed to write report column headers")
		}
	}
	return tmpl.Execute(w, responses)
}

func rm(cmd *cobra.Command, args []string) error {
	errs := []error{}
	if (len(args) > 0 && rmOptions.All) || (len(args) < 1 && !rmOptions.All) {
		return errors.New("`podman secret rm` requires one argument, or the --all flag")
	}

	manager, err := secretsManager(cmd)
	if err != nil {
		return err
	}
	toRemove := args
	if rmOptions.All {
		allSecrs, err := manager.List()
		if err != nil {
			return err
		}
		for _, secr := range allSecrs {
			toRemove = append(toRemove, secr.ID)
		}
	}

	for _, nameOrID := range toRemove {
		deletedID, err := manager.Delete(nameOrID)
		if err == nil || errors.Cause(err) == secrets.ErrNoSuchSecret {
			fmt.Println(deletedID)
			continue
		} else {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		for _, err := range errs[1:] {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		return errs[0]
	}
	return nil
}

func create(cmd *cobra.Command, args []string) error {
	name := args[0]
	options := SecretCreateOptions{}
	var err error
	path := args[1]

	var reader io.Reader
	if path == "-" || path == "/dev/stdin" {
		stat, err := os.Stdin.Stat()
		if err != nil {
			return err
		}
		if (stat.Mode() & os.ModeNamedPipe) == 0 {
			return errors.New("if `-` is used, data must be passed into stdin")
		}
		reader = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}

	data, _ := ioutil.ReadAll(reader)
	sPath, err := secretsPath(cmd)
	if err != nil {
		return err
	}
	manager, err := secretsManager(cmd)
	if err != nil {
		return err
	}

	driverOptions := make(map[string]string)
	if options.Driver == "" {
		options.Driver = "file"
	}
	if options.Driver == "file" {
		driverOptions["path"] = filepath.Join(sPath, "filedriver")
	}
	secretID, err := manager.Store(name, data, options.Driver, driverOptions)
	if err != nil {
		return err
	}
	fmt.Println(secretID)
	return nil
}

func inspect(cmd *cobra.Command, args []string) error {

	manager, err := secretsManager(cmd)
	if err != nil {
		return err
	}
	errs := []error{}
	inspected := []SecretInfoReport{}
	for _, nameOrID := range args {
		secret, err := manager.Lookup(nameOrID)
		if err != nil {
			if errors.Cause(err).Error() == "no such secret" {
				errs = append(errs, err)
				continue
			} else {
				return errors.Wrapf(err, "error inspecting secret %s", nameOrID)
			}
		}
		inspect := SecretInfoReport{
			ID:        secret.ID,
			CreatedAt: secret.CreatedAt,
			UpdatedAt: secret.CreatedAt,
			Spec: SecretSpec{
				Name: secret.Name,
				Driver: SecretDriverSpec{
					Name: secret.Driver,
				},
			},
		}
		inspected = append(inspected, inspect)
	}

	if len(inspected) > 0 {
		if cmd.Flags().Changed("format") {
			row := report.NormalizeFormat(format)
			formatted := report.EnforceRange(row)

			tmpl, err := template.New("inspect secret").Parse(formatted)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 12, 2, 2, ' ', 0)
			defer w.Flush()
			tmpl.Execute(w, inspected)
		} else {
			buf, err := json.MarshalIndent(inspected, "", "    ")
			if err != nil {
				return err
			}
			fmt.Println(string(buf))
		}
	}

	if len(errs) > 0 {
		for _, err := range errs[1:] {
			fmt.Fprintf(os.Stderr, "error inspecting secret: %v\n", err)
		}
		return errs[0]
	}
	return nil
}
