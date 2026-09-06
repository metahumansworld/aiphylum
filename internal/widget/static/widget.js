// The loader. Paste on any page:
//   <script src="https://HOST/widget.js" data-agent="a_…" async></script>
// It reads its own URL for the platform and data-agent for the agent, and
// puts a button in the corner that opens the agent's chat page in a frame.
(function () {
  var script = document.currentScript;
  if (!script) return;
  var id = script.getAttribute("data-agent");
  if (!id) return;
  var origin = new URL(script.src).origin;

  var frame = document.createElement("iframe");
  frame.src = origin + "/a/" + encodeURIComponent(id) + "/embed";
  frame.title = "Chat";
  frame.setAttribute("style",
    "position:fixed;right:24px;bottom:88px;width:360px;height:520px;max-width:calc(100vw - 32px);max-height:calc(100vh - 112px);" +
    "border:1px solid rgba(53,48,42,0.14);border-radius:12px;background:#fff;" +
    "box-shadow:0 1px 2px rgba(53,48,42,0.05),0 10px 28px rgba(53,48,42,0.12);display:none;z-index:2147483646;");

  var button = document.createElement("button");
  button.type = "button";
  button.textContent = "Chat";
  button.setAttribute("aria-expanded", "false");
  button.setAttribute("style",
    "position:fixed;right:24px;bottom:24px;height:48px;padding:0 20px;border-radius:24px;border:1px solid #cf9a24;" +
    "background:#cf9a24;color:#35302a;font:600 15px/1 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;" +
    "cursor:pointer;box-shadow:0 1px 2px rgba(53,48,42,0.05),0 10px 28px rgba(53,48,42,0.12);z-index:2147483647;");
  button.addEventListener("click", function () {
    var open = frame.style.display === "none";
    frame.style.display = open ? "block" : "none";
    button.textContent = open ? "Close" : "Chat";
    button.setAttribute("aria-expanded", String(open));
  });

  document.body.append(frame, button);
})();
