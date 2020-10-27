/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"

	v1 "github.com/apache/camel-k/pkg/apis/camel/v1"
	"github.com/apache/camel-k/pkg/builder"
	"github.com/apache/camel-k/pkg/builder/runtime"
	"github.com/apache/camel-k/pkg/trait"
	"github.com/apache/camel-k/pkg/util"
	"github.com/apache/camel-k/pkg/util/camel"
	"github.com/apache/camel-k/pkg/util/defaults"
	"github.com/apache/camel-k/pkg/util/maven"
	"github.com/scylladb/go-set/strset"
	"github.com/spf13/cobra"
)

var acceptedDependencyTypes = []string{"bom", "camel", "camel-k", "camel-quarkus", "mvn", "github"}

const defaultDependenciesDirectoryName = "dependencies"

func newCmdInspect(rootCmdOptions *RootCmdOptions) (*cobra.Command, *inspectCmdOptions) {
	options := inspectCmdOptions{
		RootCmdOptions: rootCmdOptions,
	}

	cmd := cobra.Command{
		Use:   "inspect [files to inspect]",
		Short: "Generate dependencies list given integration files.",
		Long: `Output dependencies for a list of integration files. By default this command returns the
top level dependencies only. When --all-dependencies is enabled, the transitive dependencies
will be generated by calling Maven and then copied into the directory pointed to by the
--dependencies-directory flag.`,
		PreRunE: decode(&options),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := options.validate(args); err != nil {
				return err
			}
			if err := options.initialize(args); err != nil {
				return err
			}
			if err := options.run(args); err != nil {
				fmt.Println(err.Error())
			}

			return nil
		},
		Annotations: map[string]string{
			offlineCommandLabel: "true",
		},
	}

	cmd.Flags().Bool("all-dependencies", false, "Compute transitive dependencies and move them to directory pointed to by the --dependencies-directory flag.")
	cmd.Flags().StringArrayP("dependency", "d", nil, `Additional top-level dependency with the format:
<type>:<dependency-name>
where <type> is one of {`+strings.Join(acceptedDependencyTypes, "|")+`}.`)
	cmd.Flags().String("dependencies-directory", "", "Directory that will contain all the computed dependencies. Default: <kamel-invocation-directory>/dependencies")
	cmd.Flags().StringP("output", "o", "", "Output format. One of: json|yaml")

	return &cmd, &options
}

type inspectCmdOptions struct {
	*RootCmdOptions
	AllDependencies        bool     `mapstructure:"all-dependencies"`
	OutputFormat           string   `mapstructure:"output"`
	DependenciesDirectory  string   `mapstructure:"dependencies-directory"`
	AdditionalDependencies []string `mapstructure:"dependencies"`
}

func (command *inspectCmdOptions) validate(args []string) error {
	// If no source files have been provided there is nothing to inspect.
	if len(args) == 0 {
		return errors.New("no integration files have been provided, nothing to inspect")
	}

	// Ensure source files exist.
	for _, arg := range args {
		// fmt.Printf("Validating file: %v\n", arg)
		fileExists, err := util.FileExists(arg)

		// Report any error.
		if err != nil {
			return err
		}

		// Signal file not found.
		if !fileExists {
			return errors.New("input file " + arg + " file does not exist")
		}
	}

	// Validate list of additional dependencies i.e. make sure that each dependency has
	// a valid type.
	if command.AdditionalDependencies != nil {
		for _, additionalDependency := range command.AdditionalDependencies {
			dependencyComponents := strings.Split(additionalDependency, ":")

			TypeIsValid := false
			for _, dependencyType := range acceptedDependencyTypes {
				if dependencyType == dependencyComponents[0] {
					TypeIsValid = true
				}
			}

			if !TypeIsValid {
				return errors.New("Unexpected type for user-provided dependency: " + additionalDependency + ", check command usage for valid format.")
			}

		}
	}

	// If provided, ensure that that the dependencies directory exists.
	if command.DependenciesDirectory != "" {
		dependenciesDirectoryExists, err := util.DirectoryExists(command.DependenciesDirectory)
		// Report any error.
		if err != nil {
			return err
		}

		// Signal file not found.
		if !dependenciesDirectoryExists {
			return errors.New("input file " + command.DependenciesDirectory + " file does not exist")
		}
	}

	return nil
}

func (command *inspectCmdOptions) initialize(args []string) error {
	// If --all-dependencies flag is set the dependencies directory needs to have a valid value.
	// If not provided on the command line, the value needs to be initialized with the default.
	if command.AllDependencies {
		// Move the integration dependecies to the dependencies directory.
		err := createAndSetDependenciesDirectory(command)
		if err != nil {
			return err
		}
	}
	return nil
}

func (command *inspectCmdOptions) run(args []string) error {
	// Fetch existing catalog or create new one if one does not already exist.
	catalog, err := createCamelCatalog()

	// Get top-level dependencies, this is the default behavior when no other options are provided.
	// Do not output these options when transitive options are enbled.
	dependencies, err := getTopLevelDependencies(catalog, command.OutputFormat, args, !command.AllDependencies)
	if err != nil {
		return err
	}

	// Add additional user-provided dependencies.
	if command.AdditionalDependencies != nil {
		for _, additionalDependency := range command.AdditionalDependencies {
			dependencies = append(dependencies, additionalDependency)
		}
	}

	// Top level dependencies are printed out.
	if command.AllDependencies {
		// If --all-dependencies flag is set, move all transitive dependencies in the --dependencies-directory.
		err = getTransitiveDependencies(catalog, dependencies, command)
		if err != nil {
			return err
		}
	}

	return nil
}

func getTopLevelDependencies(catalog *camel.RuntimeCatalog, format string, args []string, outputPlainText bool) ([]string, error) {
	// List of top-level dependencies.
	dependencies := strset.New()

	// Invoke the dependency inspector code for each source file.
	for _, source := range args {
		data, _, err := loadContent(source, false, false)
		if err != nil {
			return []string{}, err
		}

		sourceSpec := v1.SourceSpec{
			DataSpec: v1.DataSpec{
				Name:        path.Base(source),
				Content:     data,
				Compression: false,
			},
		}

		// Extract list of top-level dependencies.
		dependencies.Merge(trait.AddSourceDependencies(sourceSpec, catalog))
	}

	err := outputDependencies(dependencies.List(), format, outputPlainText)
	if err != nil {
		return []string{}, err
	}

	return dependencies.List(), nil
}

func generateCatalog() (*camel.RuntimeCatalog, error) {
	// A Camel catalog is requiref for this operatio.
	settings := ""
	mvn := v1.MavenSpec{
		LocalRepository: "",
	}
	runtime := v1.RuntimeSpec{
		Version:  defaults.DefaultRuntimeVersion,
		Provider: v1.RuntimeProviderMain,
	}
	providerDependencies := []maven.Dependency{}
	catalog, err := camel.GenerateCatalogCommon(settings, mvn, runtime, providerDependencies)
	if err != nil {
		return nil, err
	}

	return catalog, nil
}

func getTransitiveDependencies(
	catalog *camel.RuntimeCatalog,
	dependencies []string,
	command *inspectCmdOptions) error {

	mvn := v1.MavenSpec{
		LocalRepository: "",
	}

	// Create Maven project.
	project := runtime.GenerateProjectCommon(defaults.CamelVersion, defaults.DefaultRuntimeVersion)

	// Inject dependencies into Maven project.
	err := builder.InjectDependenciesCommon(&project, dependencies, catalog)
	if err != nil {
		return err
	}

	// Create local Maven context.
	temporaryDirectory, err := ioutil.TempDir(os.TempDir(), "maven-")
	if err != nil {
		return err
	}

	mc := maven.NewContext(temporaryDirectory, project)
	mc.LocalRepository = mvn.LocalRepository
	mc.Timeout = mvn.GetTimeout().Duration

	// Compute dependencies.
	content, err := runtime.ComputeDependenciesCommon(mc, catalog.Runtime.Version)
	if err != nil {
		return err
	}

	// Compose artifacts list.
	artifacts := []v1.Artifact{}
	artifacts, err = runtime.ProcessTransitiveDependencies(content, command.DependenciesDirectory)
	if err != nil {
		return err
	}

	// Dump dependencies in the dependencies directory and construct the list of dependencies.
	transitiveDependencies := []string{}
	for _, entry := range artifacts {
		// Copy dependencies from Maven default directory to the DependenciesDirectory.
		_, err := util.CopyFile(entry.Location, entry.Target)
		if err != nil {
			return err
		}

		transitiveDependencies = append(transitiveDependencies, entry.Target)
	}

	// Remove directory used for computing the dependencies.
	defer os.RemoveAll(temporaryDirectory)

	// Output transitive dependencies only if requested via the output format flag.
	err = outputDependencies(transitiveDependencies, command.OutputFormat, false)
	if err != nil {
		return err
	}

	return nil
}

func outputDependencies(dependencies []string, format string, outputPlainText bool) error {
	if format != "" {
		err := printDependencies(format, dependencies)
		if err != nil {
			return err
		}
	} else if outputPlainText {
		// Print output in text form.
		for _, dep := range dependencies {
			fmt.Printf("%v\n", dep)
		}
	}

	return nil
}

func printDependencies(format string, dependecies []string) error {
	switch format {
	case "yaml":
		data, err := util.DependenciesToYAML(dependecies)
		if err != nil {
			return err
		}
		fmt.Print(string(data))
	case "json":
		data, err := util.DependenciesToJSON(dependecies)
		if err != nil {
			return err
		}
		fmt.Print(string(data))
	default:
		return errors.New("unknown output format: " + format)
	}
	return nil
}

func getWorkingDirectory() (string, error) {
	currentDirectory, err := os.Getwd()
	if err != nil {
		return "", err
	}

	return currentDirectory, nil
}

func createAndSetDependenciesDirectory(command *inspectCmdOptions) error {
	if command.DependenciesDirectory == "" {
		currentDirectory, err := getWorkingDirectory()
		if err != nil {
			return err
		}

		command.DependenciesDirectory = path.Join(currentDirectory, defaultDependenciesDirectoryName)
	}

	// Create the dependencies directory if it does not already exist.
	err := util.CreateDirectory(command.DependenciesDirectory)
	if err != nil {
		return err
	}

	return nil
}

func createCamelCatalog() (*camel.RuntimeCatalog, error) {
	// Attempt to reuse existing Camel catalog if one is present.
	catalog, err := camel.MainCatalog()
	if err != nil {
		return nil, err
	}

	// Generate catalog if one was not found.
	if catalog == nil {
		catalog, err = generateCatalog()
		if err != nil {
			return nil, err
		}
	}

	return catalog, nil
}
