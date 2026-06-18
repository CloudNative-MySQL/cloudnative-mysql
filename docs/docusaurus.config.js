// @ts-check

const lightCodeTheme = require('prism-react-renderer').themes.github;
const darkCodeTheme = require('prism-react-renderer').themes.dracula;

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'CloudNative MySQL',
  tagline: 'A Kubernetes operator for Percona Server for MySQL',
  favicon: 'img/cnmysql.png',
  url: 'https://cloudnative-mysql.io',
  baseUrl: '/',
  organizationName: 'CloudNative-MySQL',
  projectName: 'cloudnative-mysql',
  onBrokenLinks: 'throw',

  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
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
        title: 'CloudNative MySQL',
        logo: {
          alt: 'CloudNative MySQL',
          src: 'img/cnmysql.png',
          srcDark: 'img/cnmysql.png',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'docsSidebar',
            position: 'left',
            label: 'Docs',
          },
          {
            to: '/api-reference',
            label: 'API',
            position: 'left',
          },
          {
            href: 'https://github.com/CloudNative-MySQL/cloudnative-mysql',
            position: 'right',
            className: 'header-github-link',
            'aria-label': 'GitHub repository',
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
                label: 'Quickstart',
                to: '/quickstart',
              },
              {
                label: 'API Reference',
                to: '/api-reference',
              },
            ],
          },
          {
            title: 'Guides',
            items: [
              {
                label: 'Replication & Failover',
                to: '/replication-failover',
              },
              {
                label: 'Backup & Recovery',
                to: '/backup-recovery',
              },
              {
                label: 'Troubleshooting',
                to: '/troubleshooting',
              },
            ],
          },
          {
            title: 'Project',
            items: [
              {
                label: 'GitHub',
                href: 'https://github.com/CloudNative-MySQL/cloudnative-mysql',
              },
              {
                label: 'Percona Server',
                href: 'https://www.percona.com/software/mysql-database/percona-server',
              },
            ],
          },
        ],
        copyright: `Copyright &copy; ${new Date().getFullYear()} The CloudNative MySQL Authors. Built with <a href="https://docusaurus.io/" target="_blank">Docusaurus</a>.<br/><small>CloudNative MySQL is an independent project, not affiliated with the CNCF or CloudNativePG.</small>`,
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
