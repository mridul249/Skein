import { createContext, useContext } from 'react';

import type { User } from './api';

/**
 * The session context.
 *
 * It holds the user object only. The access token deliberately does not live
 * in React state: state is serialisable, inspectable through devtools, and
 * ends up in error reporters. Rules.md §2.15 keeps the token in a
 * module-scoped variable inside api.ts and nowhere else.
 */
export interface SessionValue {
  user: User | null;
  setUser: (user: User | null) => void;
  /** False until the initial refresh attempt has settled. */
  ready: boolean;
}

export const SessionContext = createContext<SessionValue>({
  user: null,
  setUser: () => undefined,
  ready: false,
});

export function useSession(): SessionValue {
  return useContext(SessionContext);
}
