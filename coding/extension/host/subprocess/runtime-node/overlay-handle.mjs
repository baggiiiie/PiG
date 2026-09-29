// Mounted custom-overlay handles use the transport's narrow synchronous call.
// The host applies controls on its UI owner loop without reentering Node.
export function mountedOverlayHandle(runtime, overlay, initial) {
  let state = initial;
  const apply = next => {
    state = next;
    if (overlay.component && "focused" in overlay.component) overlay.component.focused = state.focused;
    overlay.renderFrame();
  };
  overlay.applyHandleState = apply;
  overlay.releaseHandle = () => apply({ ...state, focused: false, visible: false, bounds: undefined });
  const control = (action, hidden) => {
    if (!overlay.active) {
      if (action === "setHidden") state = { ...state, hidden };
      return;
    }
    apply(runtime.callSync("ui.custom.control", { key: overlay.key, action, hidden }));
  };
  apply(initial);
  return {
    hide: () => control("hide"),
    setHidden: hidden => control("setHidden", hidden),
    isHidden: () => state.hidden,
    focus: () => control("focus"),
    unfocus: options => {
      // pig divergence (D73): a live main-process component is not a portable focus target.
      if (options !== undefined) throw new Error("Overlay unfocus(target) requires a main-process component (D73)");
      control("unfocus");
    },
    isFocused: () => state.focused,
    getBounds: () => state.visible && state.bounds ? { ...state.bounds } : undefined,
  };
}
