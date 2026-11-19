import { createRoot } from "react-dom/client";

// The original stylesheets, unchanged. globals.css carries the design tokens
// and shared component classes used by every page.
import "../app/globals.css";
import "../app/home.css";
import "../app/images/[id]/image-page.css";

import { currentPath, resolveRoute, RootApp } from "./routes";

const route = resolveRoute(currentPath(window.location.pathname));
document.title = route.title;

const container = document.getElementById("root");
if (!container) {
  throw new Error("missing #root element in the static shell");
}

createRoot(container).render(<RootApp route={route} />);
