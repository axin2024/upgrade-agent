<?php

declare(strict_types=1);

use Illuminate\Events\Dispatcher;
use Illuminate\Foundation\Application;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Routing\Router;
use PricingLab\Pricing;

require dirname(__DIR__) . '/vendor/autoload.php';

$app = new Application(dirname(__DIR__));
$app->singleton(Pricing::class);
$pricing = $app->make(Pricing::class);
$router = new Router(new Dispatcher($app), $app);
$router->get('/orders/{order}/credits/{credit}', static function (string $order, string $credit) use ($pricing): JsonResponse {
    return new JsonResponse([
        'order' => $pricing->quote((int) $order),
        'credit' => $pricing->credit((int) $credit),
        'balance' => $pricing->balance((int) $order, (int) $credit),
    ]);
});

$response = $router->dispatch(Request::create('/orders/100/credits/-25', 'GET'));
$payload = json_decode($response->getContent(), true, flags: JSON_THROW_ON_ERROR);
$expected = ['order' => 100, 'credit' => -25, 'balance' => 75];
if ($response->getStatusCode() !== 200 || $payload !== $expected) {
    fwrite(STDERR, 'Signed credit contract failed: ' . json_encode($payload, JSON_THROW_ON_ERROR) . PHP_EOL);
    exit(1);
}

printf("Laravel %s · order=%d credit=%d balance=%d\n", $app->version(), $payload['order'], $payload['credit'], $payload['balance']);
