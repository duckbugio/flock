import type { ChangeEvent } from "react";
import { useTranslation } from "react-i18next";
import { LANGUAGES, normalizeLanguage } from "../i18n/languages";

// Compact <select>-based language switcher for the nav. The choice is persisted
// to localStorage by i18next-browser-languagedetector.
export function LanguageSwitcher() {
  const { t, i18n } = useTranslation();
  const current = normalizeLanguage(i18n.language);

  const onChange = (event: ChangeEvent<HTMLSelectElement>) => {
    void i18n.changeLanguage(event.target.value);
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
