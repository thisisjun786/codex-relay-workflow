// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/main.tsx (1-7), modified: it moves
// the fragment token out of the address before the first render.
import React from "react";
import { createRoot } from "react-dom/client";
import { bootstrapToken } from "./api.ts";
import { App } from "./App.tsx";
import "./styles.css";

// The server prints its URL with the per-run token in the fragment. Move it into
// sessionStorage and strip it from the address before anything renders, so the token
// cannot survive in a copied link, a bookmark or a Referer header.
bootstrapToken();

const el = document.getElementById("root");
if (el) createRoot(el).render(<React.StrictMode><App /></React.StrictMode>);
