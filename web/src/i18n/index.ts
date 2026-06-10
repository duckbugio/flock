import i18n from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";
import {
  FALLBACK_LANGUAGE,
  LANGUAGE_CODES,
  normalizeLanguage,
} from "./languages";
import de from "./locales/de.json";
import en from "./locales/en.json";
import es from "./locales/es.json";
import fr from "./locales/fr.json";
import ja from "./locales/ja.json";
import ptBR from "./locales/pt-BR.json";
import ru from "./locales/ru.json";
import zh from "./locales/zh.json";

export const resources = {
  en: { translation: en },
  ru: { translation: ru },
  zh: { translation: zh },
  es: { translation: es },
  de: { translation: de },
  fr: { translation: fr },
  "pt-BR": { translation: ptBR },
  ja: { translation: ja },
} as const;

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources,
    fallbackLng: FALLBACK_LANGUAGE,
    supportedLngs: [...LANGUAGE_CODES],
    // Treat "en-US", "pt", "zh-Hans"… as their supported base/variant code.
    load: "currentOnly",
    nonExplicitSupportedLngs: true,
    interpolation: { escapeValue: false },
    detection: {
      // Persisted choice wins, then the browser language.
      order: ["localStorage", "navigator"],
      lookupLocalStorage: "flock.lang",
      caches: ["localStorage"],
      convertDetectedLanguage: (lng) => normalizeLanguage(lng),
    },
  });

export default i18n;
