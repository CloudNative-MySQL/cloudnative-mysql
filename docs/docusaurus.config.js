// @ts-check

const lightCodeTheme = require('prism-react-renderer').themes.github;
const darkCodeTheme = require('prism-react-renderer').themes.dracula;

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'CNMySQL',
  tagline: 'Kubernetes operator for Cloudnative MySQL',
  favicon: 'img/favicon.svg',
  url: 'http://localhost:1313',
  baseUrl: '/',
  organizationName: 'yyewolf',
  projectName: 'cnmysql',
  onBrokenLinks: 'throw',

  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
    mermaid: true,
  },

  markdown: {
    mermaid: true,
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: 'src',
          routeBasePath: '/',
          sidebarPath: require.resolve('./sidebars.js'),
        },
        blog: false,
        theme: {
          customCss: require.resolve('./src/css/custom.css'),
        },
      }),
    ],
  ],

  themes: ['@docusaurus/theme-mermaid'],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      image: 'img/social-card.svg',
      navbar: {
        title: 'CNMySQL',
        logo: {
          alt: 'CNMySQL',
          src: 'img/logo.svg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'docsSidebar',
            position: 'left',
            label: 'Documentation',
          },
          {
            href: 'https://github.com/yyewolf/cnmysql',
            position: 'right',
            label: 'GitHub',
          },
        ],
      },
      footer: {
        style: 'light',
        links: [
          {
            title: 'Docs',
            items: [
              {
                label: 'Overview',
                to: '/',
              },
              {
                label: 'Point-In-Time Recovery',
                to: '/pitr',
              },
            ],
          },
          {
            title: 'Project',
            items: [
              {
                label: 'GitHub',
                href: 'https://github.com/yyewolf/cnmysql',
              },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} The CNMySQL Authors.`,
      },
      prism: {
        theme: lightCodeTheme,
        darkTheme: darkCodeTheme,
        additionalLanguages: ['go', 'yaml', 'bash'],
      },
      colorMode: {
        defaultMode: 'light',
        respectPrefersColorScheme: true,
      },
      mermaid: {
        theme: { light: 'neutral', dark: 'dark' },
      },
    }),
};

module.exports = config;
