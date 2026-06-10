// Supported UI languages. `en` is the source and fallback locale.
// The order here drives the language switcher and the hreflang alternates.
export const LANGUAGES = [
  { code: "en", label: "English" },
  { code: "ru", label: "Русский" },
  { code: "zh", label: "中文" },
  { code: "es", label: "Español" },
  { code: "de", label: "Deutsch" },
  { code: "fr", label: "Français" },
  { code: "pt-BR", label: "Português" },
  { code: "ja", label: "日本語" },
] as const;

export type LanguageCode = (typeof LANGUAGES)[number]["code"];

export const FALLBACK_LANGUAGE: LanguageCode = "en";

export const LANGUAGE_CODES: readonly LanguageCode[] = LANGUAGES.map(
  (l) => l.code,
);

// Maps a runtime language (e.g. "pt", "pt-br", "en-US") to a supported code.
export function normalizeLanguage(input: string | undefined): LanguageCode {
  if (!input) return FALLBACK_LANGUAGE;
  const lower = input.toLowerCase();
  // Exact (case-insensitive) match first, e.g. "pt-br" -> "pt-BR".
  const exact = LANGUAGE_CODES.find((c) => c.toLowerCase() === lower);
  if (exact) return exact;
  // Then match on the primary subtag, e.g. "en-US" -> "en", "zh-Hans" -> "zh".
  const primary = lower.split("-")[0];
  const base = LANGUAGE_CODES.find((c) => c.toLowerCase() === primary);
  return base ?? FALLBACK_LANGUAGE;
}
