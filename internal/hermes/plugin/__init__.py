"""Use documented tool overrides to admit native browser calls in the host."""

import copy
import json


def register(ctx):
    import tools.browser_use_cli
    import tools.terminal_tool
    from tools.registry import registry
    from support import bridge

    native = registry.get_entry("browser_exec")
    terminal = registry.get_entry("terminal")
    if native is None or terminal is None:
        raise RuntimeError("native_tools_unavailable")
    # get_definitions preserves the native helper description and dynamic schema.
    schemas = {entry["function"]["name"]: entry["function"] for entry in
               registry.get_definitions({"browser_exec", "terminal"}, quiet=True)}
    if "browser_exec" not in schemas or "terminal" not in schemas:
        raise RuntimeError("native_schemas_unavailable")

    def browser(args, **kwargs):
        return bridge().browser(native.handler, args)

    def denied_terminal(_args, **_kwargs):
        bridge().deny_terminal()
        return json.dumps({"error": "The generic terminal tool is unavailable in this browser worker."})

    for name, schema, handler in (("browser_exec", schemas["browser_exec"], browser),
                                  ("terminal", schemas["terminal"], denied_terminal)):
        schema = copy.deepcopy(schema)
        if name == "terminal":
            schema["description"] = "Unavailable compatibility entry. Use browser_exec for browser work."
        ctx.register_tool(name=name, toolset="commander-browser", schema=schema,
                          handler=handler, check_fn=lambda: True, override=True)
        bridge().register_handler(name, handler)
