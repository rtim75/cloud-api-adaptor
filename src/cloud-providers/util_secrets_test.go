// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package provider_test

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/aws"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/azure"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/gcp"
)

func TestManagerSecretUsage(t *testing.T) {
	for _, cloud := range []struct {
		name     string
		register func(*flag.FlagSet)
		secrets  map[string]string
	}{
		{"gcp", (&gcp.Manager{}).ParseCmd, map[string]string{"gcp-credentials": "GCP_CREDENTIALS"}},
		{"aws", (&aws.Manager{}).ParseCmd, map[string]string{
			"aws-access-key-id": "AWS_ACCESS_KEY_ID",
			"aws-secret-key":    "AWS_SECRET_ACCESS_KEY",
			"aws-session-token": "AWS_SESSION_TOKEN",
		}},
		{"azure", (&azure.Manager{}).ParseCmd, map[string]string{
			"clientid": "AZURE_CLIENT_ID",
			"secret":   "AZURE_CLIENT_SECRET",
			"tenantid": "AZURE_TENANT_ID",
		}},
	} {
		t.Run(cloud.name, func(t *testing.T) {
			for _, mode := range []string{"defaults", "help", "invalid-flag", "cli-override"} {
				t.Run(mode, func(t *testing.T) {
					var output bytes.Buffer
					flags := flag.NewFlagSet(cloud.name, flag.ContinueOnError)
					flags.SetOutput(&output)
					for _, env := range cloud.secrets {
						t.Setenv(env, "synthetic-env-"+env)
					}
					cloud.register(flags)

					var args []string
					switch mode {
					case "help":
						args = []string{"-help"}
					case "invalid-flag":
						args = []string{"-unknown-test-flag"}
					case "cli-override":
						for name := range cloud.secrets {
							args = append(args, "-"+name+"=synthetic-cli-"+name)
						}
					}
					err := flags.Parse(args)
					switch mode {
					case "help":
						if !errors.Is(err, flag.ErrHelp) {
							t.Fatal("expected help response")
						}
					case "invalid-flag":
						if err == nil {
							t.Fatal("expected unknown flag error")
						}
					default:
						if err != nil {
							t.Fatal(err)
						}
						flags.PrintDefaults()
					}
					for name, env := range cloud.secrets {
						if !strings.Contains(output.String(), "-"+name+" ") {
							t.Errorf("secret flag %s disappeared from usage", name)
						}
						if strings.Contains(output.String(), "synthetic-env-"+env) ||
							strings.Contains(output.String(), "synthetic-cli-"+name) {
							t.Errorf("usage exposed secret flag %s", name)
						}
						want := "synthetic-env-" + env
						if mode == "cli-override" {
							want = "synthetic-cli-" + name
						}
						if flags.Lookup(name).Value.(flag.Getter).Get() != want {
							t.Errorf("secret flag %s lost its configured value", name)
						}
					}
				})
			}
		})
	}
}

func TestStringWithEnvSecretDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"hardcoded", "", nil, "synthetic-default"},
		{"environment", "synthetic-env", nil, "synthetic-env"},
		{"cli", "synthetic-env", []string{"-secret=synthetic-cli"}, "synthetic-cli"},
		{"empty-cli", "synthetic-env", []string{"-secret="}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_SECRET_VALUE", tc.env)
			var secret, public string
			var output bytes.Buffer
			flags := flag.NewFlagSet("defaults", flag.ContinueOnError)
			flags.SetOutput(&output)
			reg := provider.NewFlagRegistrar(flags)
			reg.StringWithEnv(&secret, "secret", "synthetic-default", "TEST_SECRET_VALUE", "secret value", provider.Secret(), provider.Required())
			reg.StringWithEnv(&public, "public", "visible-default", "", "public value")
			if err := flags.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			flags.PrintDefaults()
			if secret != tc.want {
				t.Fatal("secret default/environment/CLI precedence changed")
			}
			for _, marker := range []string{"synthetic-default", "synthetic-env", "synthetic-cli"} {
				if strings.Contains(output.String(), marker) {
					t.Error("usage exposed secret value")
				}
			}
			if public != "visible-default" || !strings.Contains(output.String(), `(default "visible-default")`) {
				t.Error("non-secret default changed or disappeared from usage")
			}
		})
	}
}
