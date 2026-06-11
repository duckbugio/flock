import { describe, expect, it } from "vitest";
import LanguageDetector from "i18next-browser-languagedetector";
import i18next from "./index";
import { LANGUAGE_CODES, normalizeLanguage } from "./languages";

// Guards the multilingual SEO contract: the ?lng= query advertised by the
// hreflang/canonical links and the sitemap must actually select that language.
// Builds a standalone i18next instance with the same detection config as
// src/i18n/index.ts and a stubbed querystring.
function detect(search: string): string {
  const detector = new LanguageDetector();
  detector.init(
    { languageUtils: { formatLanguageCode: (l: string) => l } },
    {
      order: ["querystring", "localStorage", "navigator"],
      lookupQuerystring: "lng",
      lookupLocalStorage: "flock.lang",
      caches: [],
      convertDetectedLanguage: (lng: string) => normalizeLanguage(lng),
    },
  );
  const original = window.location.search;
  // jsdom won't let us redefine location.search directly, but history navigation
  // updates it, which is exactly what the querystring detector reads.
  window.history.replaceState(null, "", `${window.location.pathname}${search}`);
  try {
    const detected = detector.detect();
    const first = Array.isArray(detected) ? detected[0] : detected;
    return first ?? "";
  } finally {
    window.history.replaceState(
      null,
      "",
      `${window.location.pathname}${original}`,
    );
  }
}

describe("language detection via ?lng= querystring", () => {
  it("selects the language named in the ?lng= query", () => {
    expect(detect("?lng=ru")).toBe("ru");
    expect(detect("?lng=de")).toBe("de");
  });

  it("normalizes querystring variants to a supported code", () => {
    expect(detect("?lng=pt-BR")).toBe("pt-BR");
    expect(detect("?lng=en-US")).toBe("en");
  });

  it("resolves every advertised hreflang URL to its own language", () => {
    for (const code of LANGUAGE_CODES) {
      expect(detect(`?lng=${code}`)).toBe(code);
    }
  });
});

describe("i18n instance detection config", () => {
  it("checks the querystring before localStorage and navigator", () => {
    const order = (
      i18next.services.languageDetector as unknown as {
        options: { order: string[] };
      }
    ).options.order;
    expect(order).toEqual(["querystring", "localStorage", "navigator"]);
  });
});
