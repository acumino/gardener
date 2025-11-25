#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0


# Lets assume the user is targetting the runtime cluster
# Let's use the garden namespaces's gardener kubeconfig :P
seed_name="hollow-1"
root_dir="$(dirname "${0}")/.."
values_file="$root_dir/example/gardener-local/hollow-gardenlet/values.yaml"
bootstrap_token_file="$root_dir/example/gardener-local/hollow-gardenlet/secret-bootstrap-token.yaml"
garden_kubeconfig_file="$root_dir/dev/garden-kubeconfig.yaml"

generate_random_string() {
  local length=$1
  local chars='a-z0-9'
  random_string=$(tr -dc "$chars" < /dev/urandom | fold -w $length | head -n 1)
  echo $random_string
}

parse_flags() {
  while test $# -gt 0; do
    case "$1" in
      --seed_name)
        shift
        seed_name="${1:-seed_name}"
        ;;
      *)
        echo "Unknown argument: $1"
        exit 1
        ;;
    esac
    shift
  done
}

parse_flags "$@"

echo $seed_name
echo "Getting kubeconfig for Garden"
kubectl -n garden get secret/gardener -o jsonpath='{.data.kubeconfig}' | base64 --decode > $garden_kubeconfig_file
# Create bootstrap secret in the garden cluster
temp_bootstrap_file="$root_dir/dev/bootstrap-token-hollow-gardenlet.yaml"
cp $bootstrap_token_file $temp_bootstrap_file
# Name
token_id=$(generate_random_string 6)
yq eval '.metadata.name = "bootstrap-token-'$token_id'"' -i $temp_bootstrap_file
yq eval '.stringData.token-id = "'$token_id'"' -i $temp_bootstrap_file
echo "Creating bootstrap token secret in garden cluster"
kubectl apply -f $temp_bootstrap_file --kubeconfig=$garden_kubeconfig_file

# Patch values.yaml
temp_values_file="$root_dir/dev/values-hollow-gardenlet.yaml"
cp $values_file $temp_values_file
# SeedName
yq eval '.config.seedConfig.metadata.name = "'$seed_name'"' -i $temp_values_file
# ServiceAccount
yq eval '.serviceAccountName = "gardenlet-'$seed_name'"' -i $temp_values_file
# Leaseobject
yq eval '.config.leaderElection.resourceName = "gardenlet-'$seed_name'"' -i $temp_values_file
# kubeconfig secret
yq eval '.config.gardenClientConnection.kubeconfigSecret.name = "gardenlet-kubeconfig-'$seed_name'"' -i $temp_values_file
# bootstrap kubeconfig secret
yq eval '.config.gardenClientConnection.bootstrapKubeconfig.name = "gardenlet-kubeconfig-bootstrap-'$seed_name'"' -i $temp_values_file
# bootstrap token id sed magic. Please don't touch
sed -i "s/TOKEN/$token_id/g" $temp_values_file

echo "Install chart"
helm upgrade --install gardenlet-$seed_name $root_dir/charts/gardener/gardenlet --values $temp_values_file --take-ownership