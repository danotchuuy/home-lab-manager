import { createContext, useContext } from "react";
import type { ServerAccess } from "./types";

/** Whether the caller is an admin, for showing what only admins can use. The API decides for itself. */
export const AdminContext = createContext(false);

export const useAdmin = () => useContext(AdminContext);

const RANK: Record<ServerAccess, number> = { viewer: 1, operator: 2, manager: 3, admin: 4 };

/** Whether the caller's role on a server includes want. */
export function can(server: { access?: ServerAccess }, want: ServerAccess): boolean {
  return !!server.access && RANK[server.access] >= RANK[want];
}
