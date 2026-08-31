# Builtin plugin migration template

`backend/plugins/builtin` stores metadata for capabilities compiled into the
binary. A builtin capability must still register through the same Core/Registry
API as an external plugin. Its manifest is descriptive only; runtime registration
must use the real tool, command, event and permission registrars.

Required migration checklist:

1. declare stable id/version/API version and one-line description;
2. declare tools, commands, events and permission nodes;
3. register against the live host registries during enable;
4. bind every task/subscription/resource to the plugin owner;
5. unregister everything on disable/unload;
6. appear in `registry.describe`;
7. pass real invocation, denial, disable and reload tests.
