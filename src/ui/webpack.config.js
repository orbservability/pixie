const { resolve, join } = require('path');
const { execSync } = require('child_process');

const webpack = require('webpack');
const { CheckerPlugin } = require('awesome-typescript-loader');
const ArchivePlugin = require('webpack-archive-plugin');
const CaseSensitivePathsPlugin = require('case-sensitive-paths-webpack-plugin');
const HtmlWebpackHarddiskPlugin = require('html-webpack-harddisk-plugin');
const HtmlWebpackPlugin = require('html-webpack-plugin');
const utils = require('./webpack-utils');
const ReplacePlugin = require('webpack-plugin-replace');
const YAML = require('yaml');

const isDevServer = process.argv.find(v => v.includes('webpack-dev-server'));

let plugins = [
  new CheckerPlugin(),
  new CaseSensitivePathsPlugin(),
  new HtmlWebpackPlugin({
    alwaysWriteToDisk: true,
    chunks: ['main', 'manifest', 'commons', 'vendor'],
    template: 'index.html',
    filename: 'index.html',
  }),
  new HtmlWebpackPlugin({
    alwaysWriteToDisk: true,
    chunks: ['main', 'manifest', 'commons', 'vendor'],
    template: 'subdomain-index.html',
    filename: 'subdomain-index.html',
  }),
  new HtmlWebpackHarddiskPlugin(),
];

let entryFiles = [];

if (isDevServer) {
  // enable HMR globally
  plugins.push(new webpack.HotModuleReplacementPlugin());
  // prints more readable module names in the browser console on HMR updates
  plugins.push(new webpack.NamedModulesPlugin());

  entryFiles = [require.resolve('react-dev-utils/webpackHotDevClient'), 'index.tsx'];
} else {
    // Archive plugin has problems with dev server.
  plugins.push(
    new ArchivePlugin({
      output: join(resolve(__dirname, 'dist'), 'bundle'),
      format: ['tar'],
    }));

  entryFiles = ['index.tsx'];
}

var webpackConfig = {
  context: resolve(__dirname, 'src'),
  devtool: 'source-map',
  devServer: {
    contentBase: resolve(__dirname, 'dist'),
    https: true,
    disableHostCheck: true,
    hot: true,
    publicPath: '/',
    historyApiFallback: {
      rewrites: [
  {from: /logout/, to: '/subdomain-index.html'},
        {from: /login/, to: '/subdomain-index.html'},
        {from: /create-site/, to: '/subdomain-index.html'},
        {from: /vizier/, to: '/subdomain-index.html'},
        {from: /(.*)/, to: '/index.html'},
      ],
    },
    proxy: [],
  },
  entry: entryFiles,
  mode: isDevServer ? 'development' : 'production',
  module: {
    rules: [
      {
        test: /\.js[x]?$/,
        loader: require.resolve('babel-loader'),
        options: {
          cacheDirectory: true,
        },
      },
      {
        test: /\.ts[x]?$/,
        loader: require.resolve('awesome-typescript-loader'),
      },
      {
        test: /\.(jpe?g|png|gif|svg)$/i,
        loader: require.resolve('url-loader'),
        options: {
          limit: 100,
          name: 'assets/[name].[hash:8].[ext]',
        },
      },
      {
        test: /\.scss$/,
        use: [
          {
            loader: 'style-loader',
          },
          {
            loader: 'css-loader',
          },
          {
            loader: 'sass-loader',
            options: {
              includePaths: ['node_modules'],
            },
          },
        ],
      },
      {
        test: /\.css$/,
        use: ['style-loader', 'css-loader'],
      },
      {
        test: /\.toml$/,
        use: ['raw-loader'],
      },
    ],
  },
  output: {
    filename: '[name].js',
    chunkFilename: '[name].chunk.js',
    path: resolve(__dirname, 'dist'),
    publicPath: '/',
  },
  plugins: plugins,
  resolve: {
    extensions: [
      '.js',
      '.json',
      '.jsx',
      '.ts',
      '.tsx',
      '.web.js',
      '.webpack.js',
      '.png',
    ],
    modules: ['node_modules', resolve('./src'), resolve('./assets')],
  },
  optimization: {
    splitChunks: {
      cacheGroups: {
        commons: {
          chunks: 'initial',
          minChunks: 2,
          maxInitialRequests: 5, // The default limit is too small to showcase the effect
          minSize: 0, // This is example is too small to create commons chunks
        },
        vendor: {
          test: /node_modules/,
          chunks: 'initial',
          name: 'vendor',
          priority: 10,
          enforce: true,
        },
      },
    },
  },
};

module.exports = (env) => {
  if (!isDevServer) {
    return webpackConfig;
  }

  const sslDisabled = env && env.hasOwnProperty('disable_ssl') && env.disable_ssl;
  // Add the Gateway to the proxy config.
  let gatewayPath = process.env.PL_GATEWAY_URL;
  if (!gatewayPath) {
    gatewayPath =
        'http' + (sslDisabled ? '' : 's') + '://' + utils.findGatewayProxyPath();
  }

  webpackConfig.devServer.proxy.push({
    context: ['/api'],
    target: gatewayPath,
    secure: false,
  });

  // Normally, these values are replaced by Nginx. However, since we do not
  // use nginx for the dev server, we need to replace them here.
  webpackConfig.plugins.push(
    new ReplacePlugin({
      include: [
        'containers/constants.tsx',
      ],
      values: {
        __CONFIG_AUTH0_DOMAIN__: 'pixie-labs.auth0.com',
        __CONFIG_AUTH0_CLIENT_ID__: 'qaAfEHQT7mRt6W0gMd9mcQwNANz9kRup',
        __CONFIG_DOMAIN_NAME__: 'dev.withpixie.dev',
      },
    }));

  results = execSync('sops --decrypt ..\/..\/credentials\/k8s\/dev\/cloud_proxy_tls_certs.yaml');
  credsYAML = YAML.parse(results.toString());
  webpackConfig.devServer.https = {
    key: credsYAML.stringData['tls.key'],
    cert: credsYAML.stringData['tls.crt'],
  };

  return webpackConfig;
};
