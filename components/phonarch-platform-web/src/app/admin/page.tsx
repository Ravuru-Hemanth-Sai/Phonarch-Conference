"use client";

import { Dashboard } from "../dashboard";

// Product administrators use the same workspace and room surface as customers.
// The separate admin login only establishes the stronger product-admin session;
// API authorization decides which policy controls are editable.
export default function AdminPage() {
  return <Dashboard adminMode />;
}
