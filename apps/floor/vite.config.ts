import tailwindcss from '@tailwindcss/vite';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, loadEnv } from 'vite';

export default defineConfig(({ mode }) => {
	// The MES service has no CORS; in dev, /api is proxied to it (MES_HTTP_ADDR from the repo .env).
	const env = { ...loadEnv(mode, '../..', ''), ...process.env };
	const target = `http://${env.MES_HTTP_ADDR || '127.0.0.1:8100'}`;
	return {
		plugins: [
			tailwindcss(),
			sveltekit({
				compilerOptions: {
					runes: ({ filename }) => (filename.split(/[/\\]/).includes('node_modules') ? undefined : true)
				},
				adapter: adapter({ fallback: 'index.html' })
			})
		],
		server: {
			host: '127.0.0.1',
			port: 5180,
			strictPort: true,
			proxy: { '/api': { target, rewrite: (p) => p.replace(/^\/api/, '') } }
		}
	};
});
