#!/usr/bin/env python3
"""
List all resources declared in a Helm release **at a given revision** and show
whether each one currently exists in the cluster, colouring the result:
  ✓ exists  – green
  ✗ absent  – red

Usage:
    python helm_selector.py --release-name <name> --release-namespace <ns> [--revision <n>]

Dependencies:
    pip install kubernetes PyYAML
"""

import argparse
import base64
import gzip
import json
import sys
from pathlib import PurePath
from typing import Any, Iterable, Tuple

import yaml
from kubernetes import config, dynamic
from kubernetes.client import api_client
from kubernetes.dynamic.exceptions import NotFoundError, DynamicApiError

OWNER_LABEL = "owner=helm"

# ANSI colours for CLI output
GREEN = "\033[32m"
RED = "\033[31m"
RESET = "\033[0m"

# --------------------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------------------

def find_release_secret(
    dyn_client: dynamic.DynamicClient,
    release_name: str,
    release_ns: str,
    revision: int | None = None,
):
    """Return the Secret for a specific revision (or the latest) of a Helm release."""
    v1_secrets = dyn_client.resources.get(api_version="v1", kind="Secret")
    label_sel = f"{OWNER_LABEL},name={release_name}"

    try:
        candidates = v1_secrets.get(namespace=release_ns, label_selector=label_sel).items
    except NotFoundError:
        return None

    if not candidates:
        return None

    def secret_version(sec):
        lbl = sec.metadata.labels.get("version", "")
        if lbl.isdigit():
            return int(lbl)
        try:
            return int(PurePath(sec.metadata.name).suffix.lstrip("v"))
        except ValueError:
            return 0

    if revision is None:
        return max(candidates, key=secret_version)

    for sec in candidates:
        if secret_version(sec) == revision:
            return sec
    return None


def _try_decompress(data: bytes) -> bytes:
    if data.startswith(b"\x1f\x8b"):
        try:
            return gzip.decompress(data)
        except (OSError, EOFError, gzip.BadGzipFile):
            pass
    return data


def decode_release_payload(encoded: str) -> Any:
    """Decode Secret.data['release'] robustly (handles b64+gzip variants)."""
    raw = _try_decompress(base64.b64decode(encoded))
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        try:
            raw2 = _try_decompress(base64.b64decode(raw))
            return json.loads(raw2)
        except Exception as e:
            raise RuntimeError("Unable to decode Helm release payload (unsupported format)") from e


def manifest_objects(manifest_yaml: str, default_ns: str) -> Iterable[Tuple[str, str, str, str]]:
    """Yield tuples (apiVersion, kind, namespace, name)."""
    for doc in yaml.safe_load_all(manifest_yaml):
        if not isinstance(doc, dict):
            continue
        api_ver = doc.get("apiVersion")
        kind = doc.get("kind")
        meta = doc.get("metadata", {})
        name = meta.get("name")
        ns = meta.get("namespace") or default_ns
        if api_ver and kind and name:
            yield api_ver, kind, ns, name


def object_exists(
    dyn_client: dynamic.DynamicClient,
    api_version: str,
    kind: str,
    namespace: str,
    name: str,
) -> bool:
    """Return True if the object currently exists in the cluster."""
    try:
        res = dyn_client.resources.get(api_version=api_version, kind=kind)
    except (DynamicApiError, NotFoundError):
        return False

    try:
        if res.namespaced:
            res.get(name=name, namespace=namespace)
        else:
            res.get(name=name)
        return True
    except NotFoundError:
        return False


# --------------------------------------------------------------------------------------
# CLI entrypoint
# --------------------------------------------------------------------------------------

def main():
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    parser = argparse.ArgumentParser(description="List Helm release resources and highlight live status.")
    parser.add_argument("--name", "-n", required=True)
    parser.add_argument("--namespace", "-ns", required=True)
    parser.add_argument("--revision", "-r", type=int, help="Specific Helm release revision to inspect")
    args = parser.parse_args()

    dyn = dynamic.DynamicClient(api_client.ApiClient())

    secret = find_release_secret(dyn, args.name, args.namespace, args.revision)
    if not secret:
        rev_msg = f" revision={args.revision}" if args.revision is not None else " (latest)"
        sys.exit(
            f"Helm Secret for release '{args.name}'{rev_msg} not found in namespace '{args.namespace}'."
        )

    release = decode_release_payload(secret.data["release"])
    revision = release.get("version", "?")
    manifest = release.get("manifest")
    if manifest is None:
        sys.exit("[ERROR] Release payload missing 'manifest' field.")

    header = (
        f"Resources in Helm release '{args.name}' revision {revision} (namespace {args.namespace}):"
    )
    print(header)
    print("-" * len(header))

    exists_any = False
    for api_ver, kind, ns, name in sorted(manifest_objects(manifest, args.namespace)):
        live = object_exists(dyn, api_ver, kind, ns, name)
        colour = GREEN if live else RED
        status_txt = "✓ exists" if live else "✗ absent"
        exists_any = exists_any or live
        ns_prefix  = f"{ns}/" if ns else ""
        print(f"{kind:<25} {ns_prefix}{name}  -> {colour}{status_txt}{RESET}")

    if not exists_any:
        print(
            f"\n{RED}[WARNING]{RESET} None of the declared resources were found live in the cluster – release may be purged."
        )


if __name__ == "__main__":
    main()
