/**
 * Presentational homepage shared by the Next.js route and the static Go bundle.
 *
 * Extracted from app/page.tsx without any markup, class or copy change: the
 * page keeps its server-side session lookup and passes the two derived flags,
 * while the static bundle renders the same tree from /api/auth/me.
 */

type FeatureIconName = "image" | "tag" | "trash" | "archive" | "link" | "users";

const FEATURES: Array<{
  icon: FeatureIconName;
  title: string;
  description: string;
}> = [
  {
    icon: "image",
    title: "原图保存",
    description: "上传原图并保留文件内容，列表使用小预览方便浏览。",
  },
  {
    icon: "tag",
    title: "标签整理",
    description: "为图片添加标签，按主题筛选，也能批量编辑。",
  },
  {
    icon: "trash",
    title: "回收站",
    description: "误删的图片可先从回收站找回，过期后再自动清理。",
  },
  {
    icon: "archive",
    title: "打包下载",
    description: "将全部或选中的图片打包下载，留存一份副本。",
  },
  {
    icon: "link",
    title: "链接分享",
    description: "复制图片链接，放进文章、笔记或发给朋友。",
  },
  {
    icon: "users",
    title: "空间管理",
    description: "管理员可为账号设置空间额度并查看使用情况。",
  },
];

const STEPS = [
  {
    title: "上传图片",
    description: "拖入、选择多张图片，或粘贴图片链接开始上传。",
  },
  {
    title: "添加标签",
    description: "为图片归类，之后按主题快速筛选和批量整理。",
  },
  {
    title: "分享或保存",
    description: "复制图片链接分享，或把选中的图片打包下载。",
  },
];

function FeatureIcon({ name }: { name: FeatureIconName }) {
  const sharedProps = {
    "aria-hidden": true as const,
    width: 24,
    height: 24,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.7,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
  };

  switch (name) {
    case "image":
      return (
        <svg {...sharedProps}>
          <rect x="3.5" y="4.5" width="17" height="15" rx="2.5" />
          <circle cx="9" cy="10" r="1.5" />
          <path d="m4.5 17 5-4 3.2 2.5 2.2-1.8 4.6 3.6" />
        </svg>
      );
    case "tag":
      return (
        <svg {...sharedProps}>
          <path d="M20 13 13 20a2 2 0 0 1-2.8 0L4 13.8V4h9.8l6.2 6.2a2 2 0 0 1 0 2.8Z" />
          <circle cx="8.5" cy="8.5" r="1.2" />
        </svg>
      );
    case "trash":
      return (
        <svg {...sharedProps}>
          <path d="M4.5 7h15M9 7V4.5h6V7m3.5 0-.8 12H6.3L5.5 7" />
          <path d="M10 10.5v5m4-5v5" />
        </svg>
      );
    case "archive":
      return (
        <svg {...sharedProps}>
          <path d="M4 7.5h16v12H4zM3 4.5h18v3H3zM9 11h6" />
        </svg>
      );
    case "link":
      return (
        <svg {...sharedProps}>
          <path d="m9 15 6-6M7.2 17.8a4 4 0 0 1-5.6-5.6l4.1-4.1a4 4 0 0 1 5.6 0m4.7-1.9a4 4 0 0 1 5.6 5.6l-4.1 4.1a4 4 0 0 1-5.6 0" />
        </svg>
      );
    case "users":
      return (
        <svg {...sharedProps}>
          <circle cx="9" cy="8" r="3" />
          <path d="M3.5 19c.4-3 2.2-4.5 5.5-4.5s5.1 1.5 5.5 4.5M16 5.5a3 3 0 0 1 0 5.8m1.2 3.4c2 .5 3.1 1.9 3.3 4.3" />
        </svg>
      );
  }

  return null;
}

function PreviewArt({ scene }: { scene: "coast" | "sunset" | "city" | "garden" }) {
  if (scene === "coast") {
    return (
      <svg viewBox="0 0 320 180" role="img" aria-label="青绿色山海风景插画">
        <defs>
          <linearGradient id="coast-sky" x2="0" y2="1">
            <stop stopColor="#c5f1e8" />
            <stop offset="1" stopColor="#e8f8e9" />
          </linearGradient>
        </defs>
        <rect width="320" height="180" fill="url(#coast-sky)" />
        <circle cx="253" cy="43" r="19" fill="#fff2c7" />
        <path d="m0 113 61-58 48 52 47-43 78 68H0Z" fill="#83c7aa" />
        <path d="m91 124 67-60 66 63 42-34 54 39v48H0v-42Z" fill="#379a88" />
        <path d="M0 127c51-10 80 6 127 0s76-10 112-2 53 9 81 2v53H0Z" fill="#55bcb5" />
        <path d="M0 150c49-7 85 6 130 1s93-11 132-3 44 6 58 2v30H0Z" fill="#a5dfd3" />
      </svg>
    );
  }

  if (scene === "sunset") {
    return (
      <svg viewBox="0 0 320 180" role="img" aria-label="暖橙色日落插画">
        <defs>
          <linearGradient id="sunset-sky" x2="0" y2="1">
            <stop stopColor="#f9c495" />
            <stop offset="1" stopColor="#f6dfc3" />
          </linearGradient>
        </defs>
        <rect width="320" height="180" fill="url(#sunset-sky)" />
        <circle cx="220" cy="69" r="28" fill="#fff0bd" />
        <path d="M0 123c47-10 77-19 115-13s68 23 107 13 61-17 98-9v66H0Z" fill="#c97860" />
        <path d="M0 143c45-12 80-5 122 4s72 5 104-4 59-13 94-5v42H0Z" fill="#865e64" />
        <path d="M0 163c62-8 100 7 153 3s98-12 167-2v16H0Z" fill="#4e6370" />
        <path d="M181 108h78" stroke="#ffe9b2" strokeWidth="3" strokeLinecap="round" opacity=".8" />
      </svg>
    );
  }

  if (scene === "city") {
    return (
      <svg viewBox="0 0 320 180" role="img" aria-label="蓝色建筑与街道插画">
        <rect width="320" height="180" fill="#dcecf7" />
        <path d="M0 120h320v60H0Z" fill="#a9d3df" />
        <path d="M32 57h72v87H32z" fill="#568aa1" />
        <path d="M112 35h91v109h-91z" fill="#39758f" />
        <path d="M211 69h76v75h-76z" fill="#7caebe" />
        <path d="M121 45h73M42 68h52M220 81h57" stroke="#d9edf0" strokeWidth="4" opacity=".9" />
        <path d="M44 81h13v17H44zm25 0h13v17H69zm-25 31h13v17H44zm25 0h13v17H69zm63-55h15v19h-15zm29 0h15v19h-15zm-29 34h15v19h-15zm29 0h15v19h-15zm-29 34h15v19h-15zm29 0h15v19h-15zm52-13h14v17h-14zm25 0h14v17h-14z" fill="#d9edf0" />
        <path d="M0 145h320" stroke="#f6fafb" strokeWidth="3" />
      </svg>
    );
  }

  return (
    <svg viewBox="0 0 320 180" role="img" aria-label="绿色庭院与植物插画">
      <rect width="320" height="180" fill="#e5f0dc" />
      <path d="M0 120c45-20 88-12 130 0s89 14 190-4v64H0Z" fill="#b8d4a6" />
      <path d="M32 123V65h98v66M32 79h98M48 65V48h66v17" fill="none" stroke="#8d745b" strokeWidth="7" />
      <path d="M74 130c-7-26-5-46 0-68m-1 36c-16-12-21-24-20-35m22 52c15-13 19-25 17-39" fill="none" stroke="#477f63" strokeWidth="5" strokeLinecap="round" />
      <ellipse cx="55" cy="59" rx="17" ry="9" transform="rotate(34 55 59)" fill="#76aa78" />
      <ellipse cx="112" cy="70" rx="18" ry="9" transform="rotate(-31 112 70)" fill="#5d9b71" />
      <ellipse cx="177" cy="120" rx="22" ry="11" transform="rotate(-28 177 120)" fill="#679d6f" />
      <ellipse cx="233" cy="134" rx="27" ry="13" transform="rotate(22 233 134)" fill="#80b17a" />
      <circle cx="236" cy="71" r="19" fill="#f4c876" opacity=".88" />
    </svg>
  );
}

function HomePreview() {
  const triptych = [
    { name: "海边散步.webp", tag: "旅行", dims: "3840 × 2160", no: "NO. 01", scene: "coast" as const },
    { name: "橘色日落.jpg", tag: "风景", dims: "2880 × 1800", no: "NO. 02", scene: "sunset" as const },
    { name: "蓝色街角.png", tag: "建筑", dims: "2400 × 1600", no: "NO. 03", scene: "city" as const },
  ];

  return (
    <aside className="home-preview home-triptych" aria-label="图片库界面示意">
      <div className="home-triptych-grid">
        {triptych.map((image, idx) => (
          <figure
            className={`home-triptych-frame home-triptych-frame-${idx + 1}`}
            key={image.name}
          >
            <div className="home-triptych-mat">
              <div className="home-preview-art">
                <PreviewArt scene={image.scene} />
              </div>
            </div>
            <figcaption className="home-triptych-placard">
              <span className="home-triptych-no">{image.no} · {image.tag}</span>
              <span className="home-preview-filename" title={image.name}>
                {image.name}
              </span>
              <span className="home-triptych-dims">{image.dims}</span>
            </figcaption>
          </figure>
        ))}
      </div>
    </aside>
  );
}

export default function HomeView({
  isLoggedIn,
  isAdmin,
}: {
  isLoggedIn: boolean;
  isAdmin: boolean;
}) {
  const primaryLabel = isLoggedIn ? "进入工作台" : "登录 / 注册";

  return (
    <div className="home-root">
      <header className="home-header">
        <a href="/" className="home-brand" aria-label="PicBox 首页">
          PicBox
        </a>
        <nav className="home-nav" aria-label="主导航">
          {isAdmin && (
            <a href="/admin" className="home-nav-link">
              管理后台
            </a>
          )}
          <a href="/dashboard" className="home-nav-cta">
            {primaryLabel}
          </a>
        </nav>
      </header>

      <main>
        <section className="home-hero">
          <div className="home-hero-copy">
            <p className="home-eyebrow">自托管图片库</p>
            <h1>把喜欢的图片，好好收在一起</h1>
            <p className="home-lead">
              上传原图，用标签整理；需要时，分享链接或打包保存。
            </p>
            <div className="home-actions">
              <a className="home-button primary" href="/dashboard">
                {primaryLabel}
              </a>
              <a className="home-button" href="#features">
                了解功能
              </a>
            </div>
            <p className="home-note">
              注册需要管理员提供的激活码；首个账号无需激活码，并自动成为管理员。
            </p>
          </div>
          <HomePreview />
        </section>

        <section id="features" className="home-section">
          <div className="home-section-head">
            <h2>上传、整理、分享</h2>
            <p>把日常图片管理需要的功能放在一起，操作简单直接。</p>
          </div>
          <div className="home-grid">
            {FEATURES.map((feature) => (
              <article className="home-card" key={feature.title}>
                <span className="home-card-icon" aria-hidden="true">
                  <FeatureIcon name={feature.icon} />
                </span>
                <h3>{feature.title}</h3>
                <p>{feature.description}</p>
              </article>
            ))}
          </div>
        </section>

        <section className="home-section home-process">
          <div className="home-section-head">
            <h2>三步开始</h2>
            <p>从上传到分享，把图片收好、找回和带走。</p>
          </div>
          <ol className="home-steps">
            {STEPS.map((step, index) => (
              <li key={step.title}>
                <span className="home-step-index" aria-hidden="true">
                  {index + 1}
                </span>
                <h3>{step.title}</h3>
                <p>{step.description}</p>
              </li>
            ))}
          </ol>
        </section>

        <section className="home-bottom">
          <div>
            <h2>从第一张图片开始</h2>
            <p>收好日常图片，需要时快速找到、轻松分享。</p>
          </div>
          <a className="home-button primary" href="/dashboard">
            {primaryLabel}
          </a>
        </section>
      </main>
    </div>
  );
}
