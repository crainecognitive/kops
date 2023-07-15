/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kops/cmd/kops/util"
	kopsapi "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kops/util/pkg/text"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	replaceLong = templates.LongDesc(i18n.T(`
		Replace a resource desired configuration by filename or stdin.`))

	replaceExample = templates.Examples(i18n.T(`
		# Replace a cluster desired configuration using a YAML file
		kops replace -f my-cluster.yaml

		# Replace an instancegroup using YAML passed into stdin.
		cat instancegroup.yaml | kops replace -f -

		# Note, if the resource does not exist the command will error, use --force to provision resource
		kops replace -f my-cluster.yaml --force
		`))

	replaceShort = i18n.T(`Replace cluster resources.`)
)

// ReplaceOptions is the options for the command
type ReplaceOptions struct {
	// Filenames is a list of files containing resources to replace.
	Filenames []string
	// Force causes any missing rescources to be created.
	Force bool
}

// NewCmdReplace returns a new replace command
func NewCmdReplace(f *util.Factory, out io.Writer) *cobra.Command {
	options := &ReplaceOptions{}

	cmd := &cobra.Command{
		Use:               "replace {-f FILENAME}...",
		Short:             replaceShort,
		Long:              replaceLong,
		Example:           replaceExample,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunReplace(cmd.Context(), f, out, options)
		},
	}
	cmd.Flags().StringSliceVarP(&options.Filenames, "filename", "f", options.Filenames, "A list of one or more files separated by a comma.")
	cmd.MarkFlagRequired("filename")
	cmd.Flags().BoolVarP(&options.Force, "force", "", false, "Force any changes, which will also create any non-existing resource")

	return cmd
}

// RunReplace processes the replace command
func RunReplace(ctx context.Context, f *util.Factory, out io.Writer, c *ReplaceOptions) error {
	clientset, err := f.KopsClient()
	if err != nil {
		return err
	}

	vfsContext := f.VFSContext()

	for _, f := range c.Filenames {
		var contents []byte
		if f == "-" {
			contents, err = ConsumeStdin()
			if err != nil {
				return err
			}
		} else {
			contents, err = vfsContext.ReadFile(f)
			if err != nil {
				return fmt.Errorf("error reading file %q: %v", f, err)
			}
		}
		sections := text.SplitContentToSections(contents)

		for _, section := range sections {
			o, gvk, err := kopscodecs.Decode(section, nil)
			if err != nil {
				return fmt.Errorf("error parsing file %q: %v", f, err)
			}

			switch v := o.(type) {
			case *kopsapi.Cluster:
				{
					// Retrieve the current status of the cluster.  This will eventually be part of the cluster object.
					cloud, err := cloudup.BuildCloud(v)
					if err != nil {
						return err
					}
					status, err := cloud.FindClusterStatus(v)
					if err != nil {
						return err
					}

					// Check if the cluster exists already
					clusterName := v.Name
					cluster, err := clientset.GetCluster(ctx, clusterName)
					if err != nil {
						if errors.IsNotFound(err) {
							cluster = nil
						} else {
							return fmt.Errorf("error fetching cluster %q: %v", clusterName, err)
						}
					}
					if cluster == nil {
						if !c.Force {
							return fmt.Errorf("cluster %v does not exist (try adding --force flag)", clusterName)
						}

						err = cloudup.PerformAssignments(v, cloud)
						if err != nil {
							return fmt.Errorf("error populating configuration: %w", err)
						}

						_, err = clientset.CreateCluster(ctx, v)
						if err != nil {
							return fmt.Errorf("error creating cluster: %v", err)
						}
					} else {
						_, err = clientset.UpdateCluster(ctx, v, status)
						if err != nil {
							return fmt.Errorf("error replacing cluster: %v", err)
						}
					}
				}

			case *kopsapi.InstanceGroup:
				clusterName := v.ObjectMeta.Labels[kopsapi.LabelClusterName]
				if clusterName == "" {
					return fmt.Errorf("must specify %q label with cluster name to replace instanceGroup", kopsapi.LabelClusterName)
				}
				cluster, err := clientset.GetCluster(ctx, clusterName)
				if err != nil {
					if errors.IsNotFound(err) {
						return fmt.Errorf("cluster %q not found", clusterName)
					}
					return fmt.Errorf("error fetching cluster %q: %v", clusterName, err)
				}
				// check if the instancegroup exists already
				igName := v.ObjectMeta.Name
				ig, err := clientset.InstanceGroupsFor(cluster).Get(ctx, igName, metav1.GetOptions{})
				if err != nil {
					if errors.IsNotFound(err) {
						if !c.Force {
							return fmt.Errorf("instanceGroup: %v does not exist (try adding --force flag)", igName)
						}
					} else {
						return fmt.Errorf("unable to check for instanceGroup: %v", err)
					}
				}
				switch ig {
				case nil:
					klog.Infof("instanceGroup: %v was not found, creating resource now", igName)
					_, err = clientset.InstanceGroupsFor(cluster).Create(ctx, v, metav1.CreateOptions{})
					if err != nil {
						return fmt.Errorf("error creating instanceGroup: %v", err)
					}
				default:
					_, err = clientset.InstanceGroupsFor(cluster).Update(ctx, v, metav1.UpdateOptions{})
					if err != nil {
						return fmt.Errorf("error replacing instanceGroup: %v", err)
					}
				}
			case *kopsapi.SSHCredential:
				clusterName := v.ObjectMeta.Labels[kopsapi.LabelClusterName]
				if clusterName == "" {
					return fmt.Errorf("must specify %q label with cluster name to replace SSHCredential", kopsapi.LabelClusterName)
				}
				if v.Spec.PublicKey == "" {
					return fmt.Errorf("spec.PublicKey is required")
				}

				cluster, err := clientset.GetCluster(ctx, clusterName)
				if err != nil {
					return err
				}

				sshCredentialStore, err := clientset.SSHCredentialStore(cluster)
				if err != nil {
					return err
				}

				sshKeyArr := []byte(v.Spec.PublicKey)
				err = sshCredentialStore.AddSSHPublicKey(ctx, sshKeyArr)
				if err != nil {
					return fmt.Errorf("error replacing SSHCredential: %v", err)
				}
			default:
				klog.V(2).Infof("Type of object was %T", v)
				return fmt.Errorf("unhandled kind %q in %q", gvk, f)
			}
		}
	}

	return nil
}
