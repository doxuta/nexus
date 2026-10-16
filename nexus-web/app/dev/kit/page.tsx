"use client";

// /dev/kit — internal showcase of the SLDS component kit (components/slds/*, specs/038 FR-006).
// Static sample data only; not linked from any navigation. Each section names the SLDS
// blueprint it copies so a reviewer can diff it against loop/campaign/refs/blueprints/.
import { SECTIONS } from "./sections";

export default function KitPage() {
  return (
    <div className="slds-p-around_medium">
      <div className="slds-page-header slds-m-bottom_medium">
        <div className="slds-page-header__row">
          <div className="slds-page-header__col-title">
            <div className="slds-media">
              <div className="slds-media__body">
                <div className="slds-page-header__name">
                  <div className="slds-page-header__name-title">
                    <h1>
                      <span className="slds-page-header__title slds-truncate">Bộ component SLDS của Nexus</span>
                    </h1>
                  </div>
                </div>
                <p className="slds-page-header__name-meta">
                  Salesforce Lightning Design System 2.264.1 · {SECTIONS.length} nhóm · dữ liệu mẫu tĩnh
                </p>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div className="slds-grid slds-gutters">
        <nav className="slds-col slds-size_1-of-6" aria-label="Các nhóm component">
          <ul className="slds-has-dividers_bottom-space">
            {SECTIONS.map((s) => (
              <li key={s.id} className="slds-item">
                <a href={`#${s.id}`}>{s.title}</a>
              </li>
            ))}
          </ul>
        </nav>
        <div className="slds-col slds-size_5-of-6">
          {SECTIONS.map(({ id, title, blueprint, Component }) => (
            <article key={id} id={id} className="slds-card slds-m-bottom_medium">
              <div className="slds-card__header slds-grid">
                <header className="slds-media slds-media_center slds-has-flexi-truncate">
                  <div className="slds-media__body">
                    <h2 className="slds-card__header-title">{title}</h2>
                    <p className="slds-text-body_small slds-text-color_weak">Blueprint: {blueprint}</p>
                  </div>
                </header>
              </div>
              <div className="slds-card__body slds-card__body_inner">
                <Component />
              </div>
            </article>
          ))}
        </div>
      </div>
    </div>
  );
}
