import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { PublicStatus } from "./pages/PublicStatus";
import "./styles.css";

// The public status page stands alone: no app, no sign-in.
const publicStatus = window.location.pathname === "/status";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    {publicStatus ? (
      <PublicStatus />
    ) : (
      <BrowserRouter>
        <App />
      </BrowserRouter>
    )}
  </StrictMode>,
);
