import { Fragment } from "react";
import { useTranslation } from "react-i18next";
import { CodeBlock } from "../components/CodeBlock";
import { Footer } from "../components/Footer";
import { Nav } from "../components/Nav";
import { Seo } from "../components/Seo";
import { QUICKSTART_SNIPPET, SITE } from "../config";

const PIPELINE_ROLES = [
  "planner",
  "coder",
  "tester",
  "reviewer",
  "arbiter",
] as const;

const FEATURES = [
  "isolation",
  "parallel",
  "git",
  "voice",
  "auth",
  "docker",
] as const;

const FAQ_ITEMS = ["1", "2", "3", "4"] as const;

const DEMO_FLOW = ["chat", "plan", "code", "tests", "review", "PR"] as const;

export function LandingPage() {
  const { t } = useTranslation();

  return (
    <Fragment>
      <Seo />
      <Nav />

      <main id="top">
        {/* Hero */}
        <header className="hero">
          <div className="container">
            <span className="badge">{t("flock.hero.badge")}</span>
            <h1>{t("flock.hero.title")}</h1>
            <p className="hero-subtitle">{t("flock.hero.subtitle")}</p>
            <div className="hero-actions">
              <a className="btn btn-primary" href="#deploy">
                {t("flock.hero.cta_primary")}
              </a>
              <a
                className="btn btn-secondary"
                href={SITE.githubUrl}
                target="_blank"
                rel="noreferrer"
              >
                {t("flock.hero.cta_secondary")}
              </a>
            </div>
            <p className="hero-managed">
              <a href={SITE.managedUrl} target="_blank" rel="noreferrer">
                {t("flock.hero.cta_managed")}
              </a>
            </p>
          </div>
        </header>

        {/* Demo / what it is */}
        <section className="section section-tight">
          <div className="container">
            <div className="flow">
              {DEMO_FLOW.map((step, i) => (
                <Fragment key={step}>
                  {i > 0 && (
                    <span className="flow-arrow" aria-hidden="true">
                      →
                    </span>
                  )}
                  <span className="flow-chip">{step}</span>
                </Fragment>
              ))}
            </div>
            <h2 className="section-title">{t("flock.demo.title")}</h2>
            <p className="section-lead">{t("flock.demo.text")}</p>
          </div>
        </section>

        {/* How it works — the 5-role pipeline */}
        <section id="how" className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.how.title")}</h2>
            <div className="pipeline">
              {PIPELINE_ROLES.map((role, i) => (
                <div className="pipeline-step" key={role}>
                  <div className="pipeline-num">{i + 1}</div>
                  <div className="pipeline-body">
                    <span className="pipeline-role">
                      {t(`flock.roles.${role}`)}
                    </span>
                    <p className="pipeline-desc">{t(`flock.how.${role}`)}</p>
                  </div>
                </div>
              ))}
            </div>
          </div>
        </section>

        {/* Features grid */}
        <section id="features" className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.features.title")}</h2>
            <div className="grid grid-3">
              {FEATURES.map((key) => (
                <div className="card" key={key}>
                  <h3 className="card-title">
                    {t(`flock.features.${key}_title`)}
                  </h3>
                  <p className="card-text">{t(`flock.features.${key}_text`)}</p>
                </div>
              ))}
            </div>
          </div>
        </section>

        {/* Self-host vs managed split */}
        <section className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.paths.title")}</h2>
            <div className="split">
              <div className="card">
                <h3 className="card-title">{t("flock.paths.self_title")}</h3>
                <p className="card-text">{t("flock.paths.self_text")}</p>
                <a className="btn btn-secondary" href="#deploy">
                  {t("flock.paths.self_cta")}
                </a>
              </div>
              <div className="card card-active">
                <h3 className="card-title">{t("flock.paths.managed_title")}</h3>
                <p className="card-text">{t("flock.paths.managed_text")}</p>
                <a
                  className="btn btn-primary"
                  href={SITE.managedUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("flock.paths.managed_cta")}
                </a>
              </div>
            </div>
          </div>
        </section>

        {/* Quickstart */}
        <section id="deploy" className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.quickstart.title")}</h2>
            <div className="quickstart-steps">
              {(["step1", "step2", "step3"] as const).map((step, i) => (
                <div className="quickstart-step" key={step}>
                  <div className="pipeline-num">{i + 1}</div>
                  <span>{t(`flock.quickstart.${step}`)}</span>
                </div>
              ))}
            </div>
            <CodeBlock code={QUICKSTART_SNIPPET} />
          </div>
        </section>

        {/* Open source / community */}
        <section className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.oss.title")}</h2>
            <p className="section-lead">{t("flock.oss.text")}</p>
            <div className="hero-actions">
              <a
                className="btn btn-primary"
                href={SITE.githubUrl}
                target="_blank"
                rel="noreferrer"
              >
                {t("flock.oss.cta_star")}
              </a>
              <a
                className="btn btn-secondary"
                href={SITE.githubIssuesUrl}
                target="_blank"
                rel="noreferrer"
              >
                {t("flock.oss.cta_issues")}
              </a>
            </div>
          </div>
        </section>

        {/* FAQ */}
        <section className="section">
          <div className="container">
            <h2 className="section-title">{t("flock.faq.title")}</h2>
            <div className="faq-list">
              {FAQ_ITEMS.map((n) => (
                <div className="faq-item" key={n}>
                  <p className="faq-q">{t(`flock.faq.q${n}`)}</p>
                  <p className="faq-a">{t(`flock.faq.a${n}`)}</p>
                </div>
              ))}
            </div>
          </div>
        </section>

        {/* Final CTA */}
        <section className="section">
          <div className="container">
            <div className="final-cta">
              <h2>{t("flock.final.title")}</h2>
              <p>{t("flock.final.subtitle")}</p>
              <div className="final-actions">
                <a className="btn btn-primary" href="#deploy">
                  {t("flock.final.cta_primary")}
                </a>
                <a
                  className="btn btn-secondary"
                  href={SITE.managedUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("flock.final.cta_secondary")}
                </a>
              </div>
            </div>
          </div>
        </section>
      </main>

      <Footer />
    </Fragment>
  );
}
