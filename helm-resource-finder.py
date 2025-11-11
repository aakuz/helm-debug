#!/usr/bin/env python3
"""
Script to search for a Kubernetes resource across all Helm releases by reading Helm secrets directly.
Works with Helm 3 which stores release information in Kubernetes secrets.
"""

import argparse
import base64
import gzip
import zlib
import bz2
import json
import yaml
import sys
from typing import Optional, List, Dict, Any
from dataclasses import dataclass
from datetime import datetime

try:
    from kubernetes import client, config
    from kubernetes.client.rest import ApiException
except ImportError:
    print("Error: kubernetes Python client is not installed", file=sys.stderr)
    print("Install it with: pip install kubernetes", file=sys.stderr)
    sys.exit(1)


@dataclass
class ResourceMatch:
    """Data class to store information about found resources"""
    release_name: str
    namespace: str
    revision: int
    status: str
    updated: str
    found_in_manifest: bool
    resource_details: Dict[str, Any]


class HelmResourceFinder:
    def __init__(self, kind: str, name: str, namespace: Optional[str] = None):
        self.kind = kind.lower()
        self.name = name
        self.namespace = namespace
        self.matches: List[ResourceMatch] = []
        self.decode_errors = 0
        self.successful_decodes = 0
        
        # Initialize Kubernetes client
        try:
            # Try to load in-cluster config first
            config.load_incluster_config()
        except:
            # Fall back to kubeconfig
            try:
                config.load_kube_config()
            except Exception as e:
                print(f"Failed to load Kubernetes config: {e}", file=sys.stderr)
                sys.exit(1)
        
        self.v1 = client.CoreV1Api()
    
    def get_helm_secrets(self) -> List[client.V1Secret]:
        """Get all Helm release secrets from all namespaces"""
        print("Fetching Helm release secrets from cluster...")
        helm_secrets = []
        
        try:
            # Get secrets from all namespaces
            secrets = self.v1.list_secret_for_all_namespaces(
                label_selector="owner=helm"
            )
            
            for secret in secrets.items:
                # Helm 3 secrets have specific naming pattern: sh.helm.release.v1.<name>.v<version>
                if secret.metadata.name.startswith("sh.helm.release.v1."):
                    helm_secrets.append(secret)
                    
        except ApiException as e:
            print(f"Error fetching secrets: {e}", file=sys.stderr)
            return []
        
        return helm_secrets
    
    def _try_decompress(self, data: bytes) -> bytes:
        """Try to decompress data using various methods"""
        # Try gzip first (most common)
        try:
            return gzip.decompress(data)
        except:
            pass
        
        # Try zlib
        try:
            return zlib.decompress(data)
        except:
            pass
        
        # Try bz2
        try:
            return bz2.decompress(data)
        except:
            pass
        
        # If no compression worked, return as-is
        return data
    
    def decode_helm_release(self, secret: client.V1Secret) -> Optional[Dict[str, Any]]:
        """Decode Helm release data from secret robustly (handles various encoding variants)"""
        try:
            # Get the release data
            if not secret.data or "release" not in secret.data:
                return None
            
            encoded = secret.data["release"]
            
            # Method 1: Standard Helm 3 format: base64 -> gzip -> JSON
            try:
                raw = self._try_decompress(base64.b64decode(encoded))
                return json.loads(raw.decode('utf-8') if isinstance(raw, bytes) else raw)
            except (json.JSONDecodeError, UnicodeDecodeError):
                pass
            
            # Method 2: Double base64 encoding (some Helm versions do this)
            try:
                first_decode = base64.b64decode(encoded).decode('utf-8').strip()
                # Check if it looks like base64
                if all(c in 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=' for c in first_decode.replace('\n', '')):
                    second_decode = base64.b64decode(first_decode)
                    raw = self._try_decompress(second_decode)
                    return json.loads(raw.decode('utf-8') if isinstance(raw, bytes) else raw)
            except:
                pass
            
            # Method 3: No compression, just base64 -> JSON
            try:
                decoded = base64.b64decode(encoded)
                return json.loads(decoded.decode('utf-8'))
            except:
                pass
            
            # Method 4: Some tools encode the already-compressed data again
            # This handles the 'H4' signature issue you encountered
            try:
                decoded = base64.b64decode(encoded)
                # If it starts with 'H4' it might be a different encoding
                if decoded[:2] == b'H4':
                    # Try to decode it as base64 again
                    second_decode = base64.b64decode(decoded)
                    raw = self._try_decompress(second_decode)
                    return json.loads(raw.decode('utf-8') if isinstance(raw, bytes) else raw)
            except:
                pass
            
            # If all methods fail, log warning
            print(f"Warning: Could not decode secret {secret.metadata.name} (unsupported format)", file=sys.stderr)
            return None
            
        except Exception as e:
            print(f"Error decoding secret {secret.metadata.name}: {e}", file=sys.stderr)
            return None
    
    def parse_manifest_resources(self, manifest: str) -> List[Dict[str, Any]]:
        """Parse YAML manifest and extract all resources"""
        resources = []
        
        if not manifest:
            return resources
        
        try:
            # Split manifest by document separator
            documents = yaml.safe_load_all(manifest)
            
            for doc in documents:
                if doc and isinstance(doc, dict):
                    resources.append(doc)
                    
        except yaml.YAMLError as e:
            print(f"Error parsing YAML manifest: {e}", file=sys.stderr)
        
        return resources
    
    def check_resource_match(self, resource: Dict[str, Any]) -> bool:
        """Check if a resource matches the search criteria"""
        if not resource or not isinstance(resource, dict):
            return False
        
        # Check kind
        resource_kind = resource.get("kind", "").lower()
        if resource_kind != self.kind:
            return False
        
        # Check name
        metadata = resource.get("metadata", {})
        resource_name = metadata.get("name", "")
        if resource_name != self.name:
            return False
        
        # Check namespace if specified
        if self.namespace:
            resource_namespace = metadata.get("namespace", "")
            # For cluster-scoped resources, namespace might be empty
            if resource_namespace and resource_namespace != self.namespace:
                return False
        
        return True
    
    def extract_release_info(self, secret_name: str) -> tuple:
        """Extract release name and revision from secret name"""
        # Format: sh.helm.release.v1.<release-name>.v<revision>
        parts = secret_name.split(".")
        if len(parts) >= 6:
            # Join parts that might contain dots in release name
            release_name = ".".join(parts[4:-1])
            # Remove 'v' prefix from revision
            revision_str = parts[-1]
            if revision_str.startswith("v"):
                try:
                    revision = int(revision_str[1:])
                    return release_name, revision
                except ValueError:
                    pass
        
        return None, None
    
    def search_in_secret(self, secret: client.V1Secret) -> None:
        """Search for the resource in a Helm release secret"""
        secret_name = secret.metadata.name
        secret_namespace = secret.metadata.namespace
        
        # Extract release info from secret name
        release_name, revision = self.extract_release_info(secret_name)
        
        if not release_name or revision is None:
            print(f"Warning: Could not parse release info from secret {secret_name}", file=sys.stderr)
            return
        
        # Decode release data
        release_data = self.decode_helm_release(secret)
        
        if not release_data:
            self.decode_errors += 1
            return
        
        self.successful_decodes += 1
        
        # Get release info
        info = release_data.get("info", {})
        status = info.get("status", "unknown")
        first_deployed = info.get("first_deployed", "")
        last_deployed = info.get("last_deployed", "")
        
        # Format timestamp
        updated_str = last_deployed or first_deployed
        if updated_str:
            try:
                # Parse and format the timestamp
                dt = datetime.fromisoformat(updated_str.replace("Z", "+00:00"))
                updated_str = dt.strftime("%Y-%m-%d %H:%M:%S")
            except:
                pass
        
        # Get manifest
        manifest = release_data.get("manifest", "")
        
        if not manifest:
            return
        
        # Parse and check resources
        resources = self.parse_manifest_resources(manifest)
        
        for resource in resources:
            if self.check_resource_match(resource):
                match = ResourceMatch(
                    release_name=release_name,
                    namespace=secret_namespace,
                    revision=revision,
                    status=status,
                    updated=updated_str,
                    found_in_manifest=True,
                    resource_details={
                        "kind": resource.get("kind"),
                        "name": resource.get("metadata", {}).get("name"),
                        "namespace": resource.get("metadata", {}).get("namespace", "N/A"),
                        "labels": resource.get("metadata", {}).get("labels", {}),
                        "annotations": resource.get("metadata", {}).get("annotations", {})
                    }
                )
                self.matches.append(match)
                print(f"  ✓ Found in {release_name} revision {revision} (status: {status})")
    
    def search(self) -> None:
        """Main search function"""
        print(f"\n🔍 Searching for {self.kind}/{self.name}", end="")
        if self.namespace:
            print(f" in namespace '{self.namespace}'")
        else:
            print(" across all namespaces")
        print("-" * 60)
        
        # Get all Helm secrets
        secrets = self.get_helm_secrets()
        
        if not secrets:
            print("No Helm release secrets found in the cluster")
            print("\nNote: This script works with Helm 3 releases stored as Kubernetes secrets.")
            return
        
        print(f"Found {len(secrets)} Helm release secret(s)\n")
        
        # Group secrets by release
        releases_dict = {}
        for secret in secrets:
            release_name, _ = self.extract_release_info(secret.metadata.name)
            if release_name:
                if release_name not in releases_dict:
                    releases_dict[release_name] = []
                releases_dict[release_name].append(secret)
        
        print(f"Processing {len(releases_dict)} unique Helm release(s)...")
        
        # Search in each secret
        for release_name, release_secrets in releases_dict.items():
            print(f"Searching in release '{release_name}'...")
            for secret in release_secrets:
                self.search_in_secret(secret)
        
        # Print results
        self.print_results()
    
    def print_results(self) -> None:
        """Print search results"""
        print("\n" + "=" * 60)
        print("SEARCH RESULTS")
        print("=" * 60)
        
        if not self.matches:
            print(f"\n❌ Resource {self.kind}/{self.name} NOT FOUND in any Helm release")
        else:
            print(f"\n✅ Resource {self.kind}/{self.name} FOUND in {len(self.matches)} location(s):\n")
            
            # Sort matches by release name and revision
            self.matches.sort(key=lambda x: (x.release_name, -x.revision))
            
            # Group by release
            releases_dict = {}
            for match in self.matches:
                key = f"{match.release_name} (namespace: {match.namespace})"
                if key not in releases_dict:
                    releases_dict[key] = []
                releases_dict[key].append(match)
            
            for release_key, matches in releases_dict.items():
                print(f"📦 Release: {release_key}")
                for match in matches:
                    status_icon = "🟢" if match.status == "deployed" else "🟡"
                    print(f"   └─ {status_icon} Revision {match.revision} (status: {match.status})")
                    
                    if match.updated:
                        print(f"      • Updated: {match.updated}")
                    
                    # Print additional details
                    details = match.resource_details
                    if details.get("namespace") != "N/A":
                        print(f"      • Resource namespace: {details['namespace']}")
                    
                    if details.get("labels"):
                        labels_str = ", ".join([f"{k}={v}" for k, v in details['labels'].items()])
                        print(f"      • Labels: {labels_str}")
                    
                    if details.get("annotations"):
                        # Show only key names for annotations (they can be long)
                        ann_keys = list(details['annotations'].keys())
                        if ann_keys:
                            print(f"      • Annotations: {', '.join(ann_keys[:5])}")
                            if len(ann_keys) > 5:
                                print(f"        (and {len(ann_keys) - 5} more...)")
                print()
        
        print("=" * 60)
        print("STATISTICS")
        print("-" * 60)
        total_secrets = len(self.get_helm_secrets())
        print(f"Total Helm secrets found: {total_secrets}")
        print(f"Successfully decoded: {self.successful_decodes}")
        if self.decode_errors > 0:
            print(f"⚠️  Failed to decode: {self.decode_errors} (unsupported format)")
        print("=" * 60)


def main():
    parser = argparse.ArgumentParser(
        description="Search for a Kubernetes resource across all Helm releases by reading Helm secrets directly",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
This script reads Helm 3 release data directly from Kubernetes secrets without using the Helm CLI.

Examples:
  %(prog)s --kind Deployment --name nginx-deployment
  %(prog)s --kind Service --name api-service --namespace production
  %(prog)s -k ConfigMap -n redis-config
  %(prog)s -k PersistentVolumeClaim -n data-pvc -ns storage
  
Requirements:
  - Python kubernetes client: pip install kubernetes pyyaml
  - Access to Kubernetes cluster (configured kubeconfig or in-cluster)
  - Permissions to read secrets across namespaces
        """
    )
    
    parser.add_argument(
        "-k", "--kind",
        required=True,
        help="Kubernetes resource kind (e.g., Deployment, Service, ConfigMap)"
    )
    
    parser.add_argument(
        "-n", "--name",
        required=True,
        help="Name of the resource to search for"
    )
    
    parser.add_argument(
        "-ns", "--namespace",
        required=False,
        help="Namespace to filter the search (optional)"
    )
    
    parser.add_argument(
        "-v", "--verbose",
        action="store_true",
        help="Enable verbose output"
    )
    
    args = parser.parse_args()
    
    # Create finder and run search
    finder = HelmResourceFinder(
        kind=args.kind,
        name=args.name,
        namespace=args.namespace
    )
    
    try:
        finder.search()
    except KeyboardInterrupt:
        print("\n\nSearch interrupted by user")
        sys.exit(1)
    except Exception as e:
        print(f"\nError during search: {e}", file=sys.stderr)
        import traceback
        if args.verbose:
            traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()