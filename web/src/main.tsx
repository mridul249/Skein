import { StrictMode, useEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';

import { Layout } from './components/Layout';
import { Drives } from './pages/Drives';
import { Files } from './pages/Files';
import { Login } from './pages/Login';
import { Trash } from './pages/Trash';
import { SessionContext, type SessionValue } from './lib/session';
import { api, onSessionChange, type User } from './lib/api';
import './styles/index.css';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A self-hosted tool answers in milliseconds. Aggressive retries just
      // turn one visible failure into four invisible ones.
      retry: 1,
      staleTime: 5_000,
      refetchOnWindowFocus: false,
    },
  },
});

function App() {
  const [user, setUser] = useState<User | null>(null);
  const [ready, setReady] = useState(false);

  // One refresh attempt on load. The access token is deliberately not
  // persisted anywhere, so this is the only way a reload recovers a session —
  // and a token that survives a reload is a token sitting in storage where an
  // injected script can read it.
  useEffect(() => {
    let cancelled = false;
    void api.bootstrap().then((u) => {
      if (cancelled) return;
      setUser(u);
      setReady(true);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // The API layer clears the session when a refresh fails, which can happen
  // mid-session if the family was revoked. The UI has to follow.
  useEffect(() => onSessionChange((u) => setUser(u)), []);

  const value = useMemo<SessionValue>(() => ({ user, setUser, ready }), [user, ready]);

  if (!ready) {
    // A static placeholder, not a shimmer. Design.md §6: shimmer on a tool
    // that responds in 20ms is theatre.
    return (
      <div className="flex min-h-screen items-center justify-center">
        <span className="font-display text-heading text-overlay0">Skein</span>
      </div>
    );
  }

  return (
    <SessionContext.Provider value={value}>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={user ? <Navigate to="/" replace /> : <Login />} />
          <Route element={user ? <Layout /> : <Navigate to="/login" replace />}>
            <Route path="/" element={<Files />} />
            <Route path="/trash" element={<Trash />} />
            <Route path="/settings" element={<Drives />} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </BrowserRouter>
    </SessionContext.Provider>
  );
}

const root = document.getElementById('root');
if (!root) throw new Error('no #root element in the document');

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
