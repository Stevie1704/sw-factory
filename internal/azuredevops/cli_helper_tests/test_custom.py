"""Regression tests for the read-only CLI helper without credentials or HTTP."""

import importlib.util
from pathlib import Path
import sys
from types import ModuleType, SimpleNamespace
import unittest
from unittest.mock import Mock


class HelperTests(unittest.TestCase):
    """Check the SDK call contract and the deliberately narrow output."""

    def setUp(self):
        """Load the helper with SDK services replaced by inert fixtures."""
        util = ModuleType("knack.util")
        util.CLIError = ValueError
        sys.modules["knack.util"] = util
        path = Path(__file__).parents[1] / "cli_helper/azext_factory_devops/custom.py"
        spec = importlib.util.spec_from_file_location("factory_helper_custom", path)
        self.helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.helper)
        self.services = SimpleNamespace(
            get_connection_data=Mock(), get_security_client=Mock()
        )
        self.helper._services = Mock(return_value=self.services)

    def test_identity_returns_only_authenticated_account(self):
        """A response with extra fields cannot leak them to the host process."""
        self.services.get_connection_data.return_value = SimpleNamespace(
            authenticated_user=SimpleNamespace(
                properties={"Account": {"$value": "factory@example.com"}},
                unrelated_private_data="must not be returned",
            )
        )
        self.assertEqual(
            self.helper.identity("https://dev.azure.com/example"),
            {"login": "factory@example.com"},
        )
        self.services.get_connection_data.assert_called_once_with(
            "https://dev.azure.com/example"
        )

    def test_missing_account_fails_closed(self):
        """Display names and missing accounts cannot become comment owners."""
        self.services.get_connection_data.return_value = SimpleNamespace(
            authenticated_user=SimpleNamespace(properties={})
        )
        with self.assertRaisesRegex(ValueError, "account name is unavailable"):
            self.helper.identity("https://dev.azure.com/example")

    def test_permission_evaluates_caller_without_administrator_bypass(self):
        """The security SDK receives one explicit token and the exact mask."""
        client = self.services.get_security_client.return_value
        client.has_permissions.return_value = [False]
        self.assertEqual(
            self.helper.permission("https://dev.azure.com/example", "namespace", 48, "area"),
            {"granted": False},
        )
        client.has_permissions.assert_called_once_with(
            security_namespace_id="namespace",
            permissions=48,
            tokens="area",
            always_allow_administrators=False,
        )

    def test_malformed_permission_results_fail_closed(self):
        """Missing, multiple, or truthy non-boolean results never authorize."""
        client = self.services.get_security_client.return_value
        for values in (None, [], [True, True], ["false"], [1]):
            with self.subTest(values=values):
                client.has_permissions.return_value = values
                with self.assertRaisesRegex(ValueError, "invalid permission"):
                    self.helper.permission(
                        "https://dev.azure.com/example", "namespace", 48, "area"
                    )


if __name__ == "__main__":
    unittest.main()
