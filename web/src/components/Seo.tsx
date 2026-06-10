import { useEffect } from "react";
import { Helmet } from "react-helmet-async";
import { useTranslation } from "react-i18next";
import { SITE } from "../config";
import { LANGUAGE_CODES, normalizeLanguage } from "../i18n/languages";

// Seo emits localized title/description, Open Graph + Twitter card, a canonical
// link, hreflang alternates for every supported language plus x-default, and
// keeps <html lang> in sync with the active language.
export function Seo() {
  const { t, i18n } = useTranslation();
  const lang = normalizeLanguage(i18n.language);

  const title = t("flock.meta.title");
  const description = t("flock.meta.description");
  const canonical = `${SITE.baseUrl}/?lng=${lang}`;
  const ogImage = `${SITE.baseUrl}${SITE.ogImage}`;

  // Keep the document language attribute in sync for assistive tech and SEO.
  useEffect(() => {
    document.documentElement.lang = lang;
  }, [lang]);

  return (
    <Helmet htmlAttributes={{ lang }}>
      <title>{title}</title>
      <meta name="description" content={description} />
      <link rel="canonical" href={canonical} />

      <meta property="og:type" content="website" />
      <meta property="og:site_name" content="flock" />
      <meta property="og:title" content={title} />
      <meta property="og:description" content={description} />
      <meta property="og:url" content={canonical} />
      <meta property="og:image" content={ogImage} />
      <meta property="og:image:width" content="1200" />
      <meta property="og:image:height" content="630" />

      <meta name="twitter:card" content="summary_large_image" />
      <meta name="twitter:title" content={title} />
      <meta name="twitter:description" content={description} />
      <meta name="twitter:image" content={ogImage} />

      {LANGUAGE_CODES.map((code) => (
        <link
          key={code}
          rel="alternate"
          hrefLang={code}
          href={`${SITE.baseUrl}/?lng=${code}`}
        />
      ))}
      <link rel="alternate" hrefLang="x-default" href={SITE.baseUrl} />
    </Helmet>
  );
}
