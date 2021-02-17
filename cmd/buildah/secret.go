package main

import (
	"github.com/spf13/cobra"
)

var (
	secretCommand = &cobra.Command{
		Use:   "secret",
		Short: "Manage secrets",
		Long:  "Manage secrets",
		Example: `buildah secret create localhost/list
  buildah secret add localhost/list localhost/image
  buildah secret annotate --annotation A=B localhost/list localhost/image
  buildah secret annotate --annotation A=B localhost/list sha256:entrySecretDigest
  buildah secret remove localhost/list sha256:entrySecretDigest
  buildah secret inspect localhost/list
  buildah secret push localhost/list transport:destination`,
	}
)

type SecretSpec struct {
	Name   string
	Driver SecretDriverSpec
}

type SecretDriverSpec struct {
	Name    string
	Options map[string]string
}

func init() {
	secretCommand.SetUsageTemplate(UsageTemplate())
	rootCmd.AddCommand(secretCommand)
}
