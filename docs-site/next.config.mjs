import { createMDX } from 'fumadocs-mdx/next';

const withMDX = createMDX();

const repository = process.env.GITHUB_REPOSITORY?.split('/')[1] ?? '';
const owner = process.env.GITHUB_REPOSITORY_OWNER ?? '';
const isPagesRoot =
  repository.toLowerCase() === `${owner.toLowerCase()}.github.io`;
const defaultBasePath =
  process.env.GITHUB_ACTIONS && repository && !isPagesRoot
    ? `/${repository}`
    : '';
const basePath = process.env.NEXT_PUBLIC_BASE_PATH ?? defaultBasePath;

/** @type {import('next').NextConfig} */
const config = {
  assetPrefix: basePath || undefined,
  basePath: basePath || undefined,
  images: {
    unoptimized: true,
  },
  output: 'export',
  reactStrictMode: true,
  trailingSlash: true,
};

export default withMDX(config);
