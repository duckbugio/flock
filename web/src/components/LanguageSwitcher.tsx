import type { ChangeEvent } from "react";
import { useTranslation } from "react-i18next";
import { LANGUAGES, normalizeLanguage } from "../i18n/languages";

// Compact <select>-based language switcher for the nav. The choice is persisted
// to localStorage by i18next-browser-languagedetector, and the ?lng= query is
// updated so the address bar matches the canonical/hreflang URL scheme and the
// per-locale URL stays shareable.
export function LanguageSwitcher() {
  const { t, i18n } = useTranslation();
  const current = normalizeLanguage(i18n.language);

  const onChange = (event: ChangeEvent<HTMLSelectElement>) => {
    const code = normalizeLanguage(event.target.value);
    void i18n.changeLanguage(code);
    const url = new URL(window.location.href);
    url.searchParams.set("lng", code);
    window.history.replaceState(window.history.state, "", url);
  };

  return (
    <div className="lang-switch">
      <select
        aria-label={t("flock.language.label")}
        value={current}
        onChange={onChange}
      >
        {LANGUAGES.map((lang) => (
          <option key={lang.code} value={lang.code}>
            {lang.label}
          </option>
        ))}
      </select>
    </div>
  );
}
