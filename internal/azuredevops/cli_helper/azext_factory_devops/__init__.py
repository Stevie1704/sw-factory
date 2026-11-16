"""Read-only Azure CLI commands backed by the installed DevOps extension."""

from azure.cli.core import AzCommandsLoader
from azure.cli.core.commands import CliCommandType


class FactoryDevOpsCommandsLoader(AzCommandsLoader):
    """Register only the identity and calling-user permission probes."""

    def __init__(self, cli_ctx=None):
        """Bind handlers without loading or handling any credentials."""
        super().__init__(
            cli_ctx=cli_ctx,
            custom_command_type=CliCommandType(
                operations_tmpl="azext_factory_devops.custom#{}"
            ),
        )

    def load_command_table(self, args):
        """Expose the two read-only helper operations to Azure CLI."""
        with self.command_group("factory-devops") as group:
            group.custom_command("identity", "identity")
            group.custom_command("permission", "permission")
        return self.command_table

    def load_arguments(self, command):
        """Require explicit organization and permission resource selection."""
        from azure.cli.core.commands.parameters import get_three_state_flag

        with self.argument_context("factory-devops") as context:
            context.argument("organization", options_list=["--organization"], required=True)
            context.argument("detect", arg_type=get_three_state_flag(), default=False)
        with self.argument_context("factory-devops permission") as context:
            context.argument("namespace_id", options_list=["--namespace-id"], required=True)
            context.argument("permissions", type=int, required=True)
            context.argument("token", required=True)


COMMAND_LOADER_CLS = FactoryDevOpsCommandsLoader
