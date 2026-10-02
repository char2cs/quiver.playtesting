import RFB from "./novnc/core/rfb.js";

const status = document.getElementById("status");
const statusText = document.getElementById("status-text");
const screen = document.getElementById("screen");

function show(state, text) {
  status.dataset.state = state;
  statusText.textContent = text;
}

function wsURL() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const path = location.pathname.replace(/^\/s\//, "/ws/");
  return `${proto}//${location.host}${path}`;
}

function start() {
  if (!location.pathname.startsWith("/s/")) {
    show("ended", "This session is not available.");
    return;
  }
  let rfb;
  try {
    rfb = new RFB(screen, wsURL(), { wsProtocols: ["binary"] });
  } catch {
    show("ended", "This session is not available.");
    return;
  }
  rfb.scaleViewport = true;
  rfb.resizeSession = false;
  rfb.clipViewport = false;
  rfb.viewOnly = false;
  rfb.background = "#0b0d10";

  let connected = false;
  rfb.addEventListener("connect", () => {
    connected = true;
    show("connected", "Connected");
    rfb.focus();
  });
  rfb.addEventListener("disconnect", () => {
    show("ended", connected ? "Session ended." : "This session is not available.");
  });
  rfb.addEventListener("credentialsrequired", () => rfb.disconnect());
  rfb.addEventListener("securityfailure", () => rfb.disconnect());
}

start();
