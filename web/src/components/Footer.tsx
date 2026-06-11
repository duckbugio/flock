import { useTranslation } from "react-i18next";
import { SITE } from "../config";

export function Footer() {
  const { t } = useTranslation();

  return (
    <footer className="footer">
      <div className="container footer-inner">
        <p className="footer-tagline">{t("flock.footer.tagline")}</p>

        <div className="footer-links">
          <div className="footer-col">
            <span className="footer-col-title">
              {t("flock.footer.product")}
            </span>
            <a href={SITE.managedUrl} target="_blank" rel="noreferrer">
              roost
            </a>
            <a href={SITE.docsUrl} target="_blank" rel="noreferrer">
              {t("flock.footer.docs")}
            </a>
          </div>

          <div className="footer-col">
            <span className="footer-col-title">{t("flock.footer.github")}</span>
            <a href={SITE.githubUrl} target="_blank" rel="noreferrer">
              {t("flock.footer.github")}
            </a>
            <a href={SITE.licenseUrl} target="_blank" rel="noreferrer">
              {t("flock.footer.license")}
            </a>
          </div>
        </div>
      </div>
    </footer>
  );
}
