"""Use documented tool overrides to admit native browser calls in the host."""

import copy
import json


_LIGHTPANDA_GUIDANCE = (
    "Commander Lightpanda retains one page. Reuse it for first and subsequent navigation. "
    "Navigate with cdp('Page.navigate', url=..., _response_timeout=30) and check errorText. "
    "For a new document, match the navigation frameId and loaderId with id and loaderId in "
    "cdp('Page.getFrameTree')['frameTree']['frame']. Check the observed destination and the "
    "specific content requested. A site can update its path, and its title can be readable "
    "while document.readyState is still 'loading'. "
    "Extract with cdp('Runtime.evaluate', expression=..., returnByValue=True, awaitPromise=False, "
    "_response_timeout=30). Check exceptionDetails, then read reply['result']['value']. "
    "Do not call new_tab, create targets or switch tabs. If a bounded read cannot confirm the "
    "document, report that observation and stop without repeating navigation. "
    "These instructions override navigation examples in the native helper reference below.\n\n"
)


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
        if name == "browser_exec":
            schema["description"] = _LIGHTPANDA_GUIDANCE + schema["description"]
        elif name == "terminal":
            schema["description"] = "Unavailable compatibility entry. Use browser_exec for browser work."
        ctx.register_tool(name=name, toolset="commander-browser", schema=schema,
                          handler=handler, check_fn=lambda: True, override=True)
        bridge().register_handler(name, handler)
