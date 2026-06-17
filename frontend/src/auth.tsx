import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import type { Density, Me, ThemeMode } from "./types";
import { loadMe, logout as apiLogout } from "./api";

interface AuthState {
  me: Me;
  loading: boolean;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
}

const DEFAULT_ME: Me = { authenticated: false, auth_enabled: false };

const AuthContext = createContext<AuthState>({
  me: DEFAULT_ME,
  loading: true,
  refresh: async () => {},
  signOut: async () => {},
});

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me>(DEFAULT_ME);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    const next = await loadMe();
    setMe(next);
    setLoading(false);
  }, []);

  const signOut = useCallback(async () => {
    await apiLogout();
    await refresh();
  }, [refresh]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const value = useMemo(() => ({ me, loading, refresh, signOut }), [me, loading, refresh, signOut]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  return useContext(AuthContext);
}

/**
 * isOwner returns true iff auth is enabled, the viewer is signed in, and the
 * viewer's GitHub login matches the room's owner. Anonymous rooms have
 * `owner === null` → never owner. Auth-disabled installs always return false
 * (the owner-only UI surfaces should stay hidden).
 */
export function isOwnerOf(me: Me, ownerLogin: string | null | undefined): boolean {
  if (!ownerLogin) return false;
  if (!me.authenticated) return false;
  return me.user.login === ownerLogin;
}

/**
 * isAdmin returns true iff the viewer is signed in and the server flagged
 * them as a cross-room admin (login in AGENT_ROOM_ADMINS). Admins manage and
 * enter every room as if they owned it.
 */
export function isAdmin(me: Me): boolean {
  return me.authenticated && me.is_admin === true;
}

/* ── Theme ──────────────────────────────────────────────────────── */

interface ThemeState {
  theme: ThemeMode;
  setTheme: (t: ThemeMode) => void;
  density: Density;
  setDensity: (d: Density) => void;
  accentHue: string;
  setAccentHue: (h: string) => void;
}

const ThemeContext = createContext<ThemeState>({
  theme: "paper",
  setTheme: () => {},
  density: "regular",
  setDensity: () => {},
  accentHue: "152",
  setAccentHue: () => {},
});

const THEME_KEY = "agent-room.theme";
const DENSITY_KEY = "agent-room.density";
const ACCENT_KEY = "agent-room.accent-h";

const VALID_THEMES: ThemeMode[] = ["paper", "operator", "signal"];
const VALID_DENSITY: Density[] = ["compact", "regular", "comfy"];

function readTheme(): ThemeMode {
  const stored = localStorage.getItem(THEME_KEY);
  if (stored && (VALID_THEMES as string[]).includes(stored)) return stored as ThemeMode;
  return "paper";
}

function readDensity(): Density {
  const stored = localStorage.getItem(DENSITY_KEY);
  if (stored && (VALID_DENSITY as string[]).includes(stored)) return stored as Density;
  return "regular";
}

function readAccent(): string {
  const stored = localStorage.getItem(ACCENT_KEY);
  if (stored && /^\d{1,3}$/.test(stored)) return stored;
  return "152";
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<ThemeMode>(() => readTheme());
  const [density, setDensityState] = useState<Density>(() => readDensity());
  const [accentHue, setAccentHueState] = useState<string>(() => readAccent());

  useEffect(() => {
    document.body.classList.add("dm");
    document.documentElement.setAttribute("data-theme", theme);
    localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  useEffect(() => {
    document.documentElement.setAttribute("data-density", density);
    localStorage.setItem(DENSITY_KEY, density);
  }, [density]);

  useEffect(() => {
    document.documentElement.style.setProperty("--accent-h", accentHue);
    localStorage.setItem(ACCENT_KEY, accentHue);
  }, [accentHue]);

  const value = useMemo<ThemeState>(
    () => ({
      theme,
      setTheme: setThemeState,
      density,
      setDensity: setDensityState,
      accentHue,
      setAccentHue: setAccentHueState,
    }),
    [theme, density, accentHue],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeState {
  return useContext(ThemeContext);
}
