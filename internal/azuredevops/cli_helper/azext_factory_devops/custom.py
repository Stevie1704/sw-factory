"""Keep authentication inside the official Azure DevOps CLI connection."""

import sys


def _services(organization, detect):
    """Load the installed extension while preserving its credential store."""
    from azure.cli.core.extension import get_extension
    from knack.util import CLIError

    if detect or not organization.startswith("https://dev.azure.com/"):
        raise CLIError("Factory requires an explicit Azure DevOps organization")
    # The CLI loads each extension's dependencies from its own directory.
    # Use the installed package rather than vendoring its SDK or credentials.
    extension_path = get_extension("azure-devops").path
    if extension_path not in sys.path:
        sys.path.insert(0, extension_path)
    from azext_devops.dev.common import services

    return services


def identity(organization, detect=False):
    """Return the authenticated account name without exposing credentials."""
    from knack.util import CLIError

    caller = _services(organization, detect).get_connection_data(organization).authenticated_user
    account = (caller.properties or {}).get("Account", {}).get("$value", "").strip()
    if not account or any(character in account for character in "\x00\r\n/"):
        raise CLIError("The authenticated Azure DevOps account name is unavailable")
    return {"login": account}


def permission(organization, namespace_id, permissions, token, detect=False):
    """Evaluate the caller's effective permission with administrator bypass off."""
    from knack.util import CLIError

    if permissions <= 0 or not token:
        raise CLIError("A positive permission mask and one resource token are required")
    client = _services(organization, detect).get_security_client(organization)
    values = client.has_permissions(
        security_namespace_id=namespace_id,
        permissions=permissions,
        tokens=token,
        always_allow_administrators=False,
    )
    if not isinstance(values, list) or len(values) != 1 or type(values[0]) is not bool:
        raise CLIError("Azure DevOps returned an invalid permission evaluation")
    return {"granted": values[0]}
