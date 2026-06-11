import { describe, expect, it } from "vitest";
import { LANGUAGE_CODES, normalizeLanguage } from "./languages";
import { resources } from "./index";

describe("normalizeLanguage", () => {
  it("matches an exact supported code case-insensitively", () => {
    expect(normalizeLanguage("pt-br")).toBe("pt-BR");
    expect(normalizeLanguage("RU")).toBe("ru");
  });

  it("falls back to the primary subtag", () => {
    expect(normalizeLanguage("en-US")).toBe("en");
    expect(normalizeLanguage("zh-Hans")).toBe("zh");
    expect(normalizeLanguage("de-AT")).toBe("de");
  });

  it("falls back to English for unknown or empty input", () => {
    expect(normalizeLanguage("xx")).toBe("en");
    expect(normalizeLanguage(undefined)).toBe("en");
  });
});

describe("locale resources", () => {
  it("ships a translation bundle for every supported language", () => {
    for (const code of LANGUAGE_CODES) {
      expect(resources[code]).toBeDefined();
    }
  });

  it("keeps the same key structure across every locale", () => {
    const keysOf = (obj: unknown, prefix = ""): string[] => {
      if (typeof obj !== "object" || obj === null) return [prefix];
      return Object.entries(obj).flatMap(([k, v]) =>
        keysOf(v, prefix ? `${prefix}.${k}` : k),
      );
    };

    const enKeys = keysOf(resources.en.translation).sort();
    for (const code of LANGUAGE_CODES) {
      const keys = keysOf(resources[code].translation).sort();
      expect(keys, `locale ${code} key set`).toEqual(enKeys);
    }
  });
});
